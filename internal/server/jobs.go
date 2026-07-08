package server

import (
	"fmt"
	"log"

	"centralbackup/internal/proto"
	"centralbackup/internal/server/store"
)

// Jobs are editable from both the server GUI and each agent's local
// agent.yaml. Every accepted change bumps Job.Version; on conflict the
// higher version wins (last-write-wins). After any change the server pushes
// the authoritative full job set back to the agent, which rewrites its
// local file — so the file always reflects live config.

// pushJobs sends the authoritative job set to an agent (no-op if offline).
func (s *Server) pushJobs(agentID string) {
	rows, err := s.store.ListJobs(agentID)
	if err != nil {
		log.Printf("pushJobs %s: %v", agentID, err)
		return
	}
	jobs := make([]proto.Job, 0, len(rows))
	for _, r := range rows {
		jobs = append(jobs, r.Job)
	}
	s.hub.Send(agentID, proto.MsgJobsUpdate, proto.JobsUpdate{Jobs: jobs})
}

// handleJobsSync applies client-side job creations/edits.
func (s *Server) handleJobsSync(agentID string, sync proto.JobsSync) error {
	changed := false
	for _, j := range sync.Jobs {
		j.AgentID = agentID
		if err := validateJob(&j); err != nil {
			log.Printf("agent %s: rejected job %q: %v", agentID, j.Name, err)
			continue
		}
		if j.ID == "" {
			j.ID = store.NewID()
			j.Origin = proto.OriginClient
			j.Version = 1
			if err := s.store.SaveJob(j); err != nil {
				return err
			}
			changed = true
			continue
		}
		cur, err := s.store.GetJob(j.ID)
		if err == store.ErrNotFound || (err == nil && cur.Job.AgentID != agentID) {
			continue // unknown or foreign job id: ignore
		}
		if err != nil {
			return err
		}
		if j.Version > cur.Job.Version {
			j.Origin = cur.Job.Origin // origin never changes after creation
			if err := s.store.SaveJob(j); err != nil {
				return err
			}
			changed = true
		}
	}
	// Always push back: assigns IDs to new jobs and corrects stale clients.
	_ = changed
	s.pushJobs(agentID)
	return nil
}

func (s *Server) handleAgentJobDelete(agentID, jobID string) error {
	cur, err := s.store.GetJob(jobID)
	if err == store.ErrNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	if cur.Job.AgentID != agentID {
		return nil
	}
	if err := s.store.DeleteJob(jobID); err != nil {
		return err
	}
	s.pushJobs(agentID)
	return nil
}

func validateJob(j *proto.Job) error {
	if j.Name == "" {
		return fmt.Errorf("job name is required")
	}
	if len(j.Paths) == 0 {
		return fmt.Errorf("at least one path is required")
	}
	if j.Schedule != "" {
		if _, err := cronParser.Parse(j.Schedule); err != nil {
			return fmt.Errorf("invalid schedule %q: %w", j.Schedule, err)
		}
	}
	return nil
}

// scheduledMode picks full or incremental for an automatic run: every
// FullEvery-th run is a full (which re-reads and re-verifies every file);
// the first-ever run of a job is always effectively full.
func (s *Server) scheduledMode(row *store.JobRow) string {
	if _, err := s.store.LatestSnapshot(row.Job.ID); err == store.ErrNotFound {
		return proto.ModeFull
	}
	if row.Job.FullEvery > 0 && row.RunCount%int64(row.Job.FullEvery) == 0 {
		return proto.ModeFull
	}
	return proto.ModeIncremental
}

// startBackup creates a run and dispatches it to the agent. mode may be
// "full", "incremental" or "" (= scheduled mode selection). If the agent is
// offline and queue is true, the run is queued and dispatched on reconnect;
// otherwise an error is returned.
func (s *Server) startBackup(row *store.JobRow, mode string, queue bool) (runID string, err error) {
	if !row.Job.Enabled {
		return "", fmt.Errorf("job is disabled")
	}
	if mode == "" {
		mode = s.scheduledMode(row)
	}
	online := s.hub.Online(row.Job.AgentID)
	if !online && !queue {
		return "", fmt.Errorf("agent is offline")
	}

	status := proto.RunRunning
	if !online {
		// Avoid piling up duplicate queued runs for the same job.
		queued, err := s.store.QueuedRuns(row.Job.AgentID)
		if err != nil {
			return "", err
		}
		for _, q := range queued {
			if q.JobID == row.Job.ID {
				return q.ID, nil
			}
		}
		status = proto.RunQueued
	}

	runID, err = s.store.CreateRun(row.Job.ID, row.Job.AgentID, mode, status)
	if err != nil {
		return "", err
	}
	s.store.IncrementRunCount(row.Job.ID)
	if online {
		s.sendRunBackup(runID, row.Job, mode)
	}
	return runID, nil
}

func (s *Server) sendRunBackup(runID string, job proto.Job, mode string) {
	prevID := ""
	if prev, err := s.store.LatestSnapshot(job.ID); err == nil {
		prevID = prev.ID
	}
	ok := s.hub.Send(job.AgentID, proto.MsgRunBackup, proto.RunBackup{
		RunID: runID, Job: job, Mode: mode, PrevSnapshotID: prevID,
	})
	if !ok {
		s.store.FinishRun(runID, proto.RunError, "", "failed to dispatch to agent", proto.RunStats{})
	}
}

// startRestore dispatches a restore to the agent (requires it online).
func (s *Server) startRestore(sn *store.Snapshot, paths []string, targetDir string, overwrite bool) (string, error) {
	if !s.hub.Online(sn.AgentID) {
		return "", fmt.Errorf("agent is offline")
	}
	runID, err := s.store.CreateRun(sn.JobID, sn.AgentID, proto.ModeRestore, proto.RunRunning)
	if err != nil {
		return "", err
	}
	ok := s.hub.Send(sn.AgentID, proto.MsgRestore, proto.Restore{
		RunID: runID, SnapshotID: sn.ID, Paths: paths, TargetDir: targetDir, Overwrite: overwrite,
	})
	if !ok {
		s.store.FinishRun(runID, proto.RunError, "", "failed to dispatch to agent", proto.RunStats{})
		return "", fmt.Errorf("failed to dispatch to agent")
	}
	return runID, nil
}

// dispatchPendingWork runs when an agent (re)connects: sends queued runs
// and fires catch-up backups for schedules missed while offline.
func (s *Server) dispatchPendingWork(agentID string) {
	queued, err := s.store.QueuedRuns(agentID)
	if err == nil {
		for _, run := range queued {
			row, err := s.store.GetJob(run.JobID)
			if err != nil {
				s.store.FinishRun(run.ID, proto.RunError, "", "job no longer exists", proto.RunStats{})
				continue
			}
			s.store.MarkRunStarted(run.ID)
			s.sendRunBackup(run.ID, row.Job, run.Mode)
		}
	}

	rows, err := s.store.ListJobs(agentID)
	if err != nil {
		return
	}
	for _, row := range rows {
		if row.CatchupPending && row.Job.Enabled {
			s.store.SetCatchupPending(row.Job.ID, false)
			if _, err := s.startBackup(&row, "", false); err != nil {
				log.Printf("catch-up run for job %s: %v", row.Job.ID, err)
			}
		}
	}
}
