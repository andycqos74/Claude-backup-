package agent

import (
	"context"
	"log"
	"net/http"
	"runtime"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"centralbackup/internal/proto"
)

// Version is stamped at build time via -ldflags.
var Version = "dev"

// Agent is the long-running client daemon: it keeps an outbound WebSocket
// to the server, executes backup/restore commands, and syncs the local
// agent.yaml job configuration both ways.
type Agent struct {
	stateDir   string
	configPath string
	creds      *Credentials
	client     *serverClient

	mu   sync.Mutex // guards ws writes
	ws   *websocket.Conn
	runs sync.Mutex // serialises backup/restore runs

	cfg *localConfig // agent.yaml state (see config.go)
}

func New(stateDir, configPath string) (*Agent, error) {
	creds, err := LoadCredentials(stateDir)
	if err != nil {
		return nil, err
	}
	a := &Agent{
		stateDir:   stateDir,
		configPath: configPath,
		creds:      creds,
		client:     newServerClient(creds),
	}
	a.cfg = newLocalConfig(a)
	return a, nil
}

// Run connects to the server and keeps reconnecting with backoff until ctx
// is cancelled.
func (a *Agent) Run(ctx context.Context) error {
	go a.cfg.watch(ctx)

	backoff := time.Second
	for {
		if err := a.connectAndServe(ctx); err != nil {
			log.Printf("connection lost: %v (reconnecting in %s)", err, backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

func (a *Agent) connectAndServe(ctx context.Context) error {
	wsURL := "wss" + a.creds.ServerURL[len("https"):] + "/api/agent/ws"
	dialer := websocket.Dialer{
		TLSClientConfig:  pinnedTLSConfig(a.creds.Fingerprint),
		HandshakeTimeout: 30 * time.Second,
	}
	hdr := http.Header{}
	hdr.Set("X-Agent-Id", a.creds.AgentID)
	hdr.Set("X-Agent-Key", a.creds.Secret)
	ws, _, err := dialer.DialContext(ctx, wsURL, hdr)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.ws = ws
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.ws = nil
		a.mu.Unlock()
		ws.Close()
	}()

	// The server pings every 30s; treat 90s of silence as a dead link.
	resetDeadline := func() { ws.SetReadDeadline(time.Now().Add(90 * time.Second)) }
	resetDeadline()
	ws.SetPingHandler(func(data string) error {
		resetDeadline()
		a.mu.Lock()
		defer a.mu.Unlock()
		ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
		return ws.WriteMessage(websocket.PongMessage, []byte(data))
	})

	hostname := hostnameOrEmpty()
	if err := a.send(proto.MsgHello, proto.Hello{
		Hostname: hostname, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: Version,
	}); err != nil {
		return err
	}
	log.Printf("connected to %s", a.creds.ServerURL)

	// Push any local config edits made while offline.
	go a.cfg.syncNow()

	// Watch ctx so shutdown interrupts the blocking read.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			ws.Close()
		case <-done:
		}
	}()

	for {
		var env proto.Envelope
		if err := ws.ReadJSON(&env); err != nil {
			return err
		}
		resetDeadline()
		a.handleMessage(env)
	}
}

func (a *Agent) send(msgType string, payload any) error {
	env, err := proto.Wrap(msgType, payload)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ws == nil {
		return errNotConnected
	}
	a.ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return a.ws.WriteJSON(env)
}

func (a *Agent) handleMessage(env proto.Envelope) {
	switch env.Type {
	case proto.MsgRunBackup:
		cmd, err := unmarshalMsg[proto.RunBackup](env.Data)
		if err != nil {
			log.Printf("bad run_backup message: %v", err)
			return
		}
		go a.runBackup(cmd)

	case proto.MsgRestore:
		cmd, err := unmarshalMsg[proto.Restore](env.Data)
		if err != nil {
			log.Printf("bad restore message: %v", err)
			return
		}
		go a.runRestore(cmd)

	case proto.MsgJobsUpdate:
		upd, err := unmarshalMsg[proto.JobsUpdate](env.Data)
		if err != nil {
			log.Printf("bad jobs_update message: %v", err)
			return
		}
		a.cfg.applyServerJobs(upd.Jobs)

	default:
		log.Printf("unknown message type %q", env.Type)
	}
}

// ---- run event helpers ----

func (a *Agent) runLog(runID, level, msg string) {
	log.Printf("[run %s] %s: %s", runID, level, msg)
	a.send(proto.MsgRunLog, proto.RunLog{RunID: runID, Level: level, Message: msg})
}

func (a *Agent) runDone(d proto.RunDone) {
	if err := a.send(proto.MsgRunDone, d); err != nil {
		log.Printf("[run %s] could not report completion: %v", d.RunID, err)
	}
}
