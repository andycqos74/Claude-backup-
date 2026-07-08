package server

import (
	"log"
	"time"

	"github.com/robfig/cron/v3"
)

// Standard 5-field cron expressions (minute hour dom month dow).
var cronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)

// runScheduler evaluates every enabled job's cron expression on a short
// tick. When a job comes due: dispatch if the agent is online, otherwise
// mark it for catch-up on reconnect (if the job opted in).
func (s *Server) runScheduler() {
	const tick = 30 * time.Second
	last := time.Now()
	for now := range time.Tick(tick) {
		rows, err := s.store.ListJobs("")
		if err != nil {
			log.Printf("scheduler: list jobs: %v", err)
			continue
		}
		for _, row := range rows {
			job := row.Job
			if !job.Enabled || job.Schedule == "" {
				continue
			}
			sched, err := cronParser.Parse(job.Schedule)
			if err != nil {
				continue // validated at save time; ignore corrupt specs
			}
			next := sched.Next(last)
			if next.After(now) {
				continue
			}
			if s.hub.Online(job.AgentID) {
				if _, err := s.startBackup(&row, "", false); err != nil {
					log.Printf("scheduler: job %s (%s): %v", job.Name, job.ID, err)
				} else {
					log.Printf("scheduler: started job %s (%s)", job.Name, job.ID)
				}
			} else if job.Catchup {
				s.store.SetCatchupPending(job.ID, true)
				log.Printf("scheduler: job %s (%s) due but agent offline; catch-up pending", job.Name, job.ID)
			} else {
				log.Printf("scheduler: job %s (%s) due but agent offline; skipped", job.Name, job.ID)
			}
		}
		last = now
	}
}

// runMaintenance applies retention policies and garbage-collects
// unreferenced blobs once a day (first pass shortly after startup).
func (s *Server) runMaintenance() {
	timer := time.NewTimer(5 * time.Minute)
	for {
		<-timer.C
		if err := s.PruneAndGC(); err != nil {
			log.Printf("maintenance: %v", err)
		}
		timer.Reset(24 * time.Hour)
	}
}
