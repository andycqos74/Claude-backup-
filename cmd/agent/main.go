// backup-agent is the client-side agent: it enrolls with the central
// server, keeps an outbound TLS connection for commands, executes backups
// and restores, and lets jobs be managed locally via `backup-agent job` /
// agent.yaml.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"

	"centralbackup/internal/agent"
)

func defaultStateDir() string {
	if v := os.Getenv("CB_STATE_DIR"); v != "" {
		return v
	}
	if runtime.GOOS == "windows" {
		return `C:\ProgramData\BackupAgent`
	}
	return "/var/lib/backup-agent"
}

func defaultConfigPath(stateDir string) string {
	if v := os.Getenv("CB_CONFIG"); v != "" {
		return v
	}
	return filepath.Join(stateDir, "agent.yaml")
}

func usage() {
	fmt.Fprintf(os.Stderr, `backup-agent %s — central backup client

Usage:
  backup-agent enroll --server https://host:8443 --token TOKEN [--fingerprint FP] [--name NAME]
  backup-agent run                     run the agent daemon (foreground)
  backup-agent job list                list backup jobs
  backup-agent job add --name N --path P [--path P2] [options]
  backup-agent job rm NAME|ID          delete a job (synced to server)
  backup-agent job enable|disable NAME|ID
  backup-agent fingerprint             show the pinned server fingerprint
  backup-agent service install|uninstall|start|stop   (Windows only)
  backup-agent version

Common flags:
  --state-dir DIR   agent state directory (default %s, env CB_STATE_DIR)
  --config FILE     job config file (default <state-dir>/agent.yaml, env CB_CONFIG)
`, agent.Version, defaultStateDir())
	os.Exit(2)
}

func main() {
	log.SetFlags(log.LstdFlags)
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "enroll":
		cmdEnroll(os.Args[2:])
	case "run":
		cmdRun(os.Args[2:])
	case "job":
		cmdJob(os.Args[2:])
	case "fingerprint":
		cmdFingerprint(os.Args[2:])
	case "service":
		cmdService(os.Args[2:])
	case "version":
		fmt.Println(agent.Version)
	default:
		usage()
	}
}

func commonFlags(fs *flag.FlagSet) (stateDir, config *string) {
	stateDir = fs.String("state-dir", defaultStateDir(), "agent state directory")
	config = fs.String("config", "", "job config file (default <state-dir>/agent.yaml)")
	return
}

func resolveConfig(stateDir, config string) string {
	if config != "" {
		return config
	}
	return defaultConfigPath(stateDir)
}

func cmdEnroll(args []string) {
	fs := flag.NewFlagSet("enroll", flag.ExitOnError)
	server := fs.String("server", "", "server URL, e.g. https://backup.example.com:8443")
	token := fs.String("token", "", "one-time enrollment token from the server GUI")
	fingerprint := fs.String("fingerprint", "", "expected server TLS certificate SHA-256 fingerprint")
	name := fs.String("name", "", "display name for this client (default: hostname)")
	stateDir, _ := commonFlags(fs)
	fs.Parse(args)
	if *server == "" || *token == "" {
		fmt.Fprintln(os.Stderr, "enroll: --server and --token are required")
		os.Exit(2)
	}
	*server = strings.TrimRight(*server, "/")

	fp, err := agent.FetchFingerprint(*server)
	if err != nil {
		log.Fatalf("cannot reach server: %v", err)
	}
	if *fingerprint != "" {
		want := strings.ToLower(strings.ReplaceAll(*fingerprint, ":", ""))
		if fp != want {
			log.Fatalf("SERVER FINGERPRINT MISMATCH!\n  expected: %s\n  got:      %s\nRefusing to enroll — possible man-in-the-middle.", want, fp)
		}
	} else {
		fmt.Printf("Server TLS fingerprint (trust-on-first-use): %s\n", fp)
		fmt.Println("Verify it against the value shown in the server GUI (Settings page).")
	}

	creds, err := agent.Enroll(*server, fp, *token, *name)
	if err != nil {
		log.Fatalf("enrollment failed: %v", err)
	}
	if err := creds.Save(*stateDir); err != nil {
		log.Fatalf("could not save credentials: %v", err)
	}
	fmt.Printf("Enrolled successfully as agent %s.\nState directory: %s\nStart the agent with: backup-agent run\n",
		creds.AgentID, *stateDir)
}

func cmdRun(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	stateDir, config := commonFlags(fs)
	fs.Parse(args)

	if maybeRunAsWindowsService(*stateDir, resolveConfig(*stateDir, *config)) {
		return
	}

	a, err := agent.New(*stateDir, resolveConfig(*stateDir, *config))
	if err != nil {
		log.Fatalf("not enrolled yet? %v (run `backup-agent enroll` first)", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Run(ctx); err != nil && ctx.Err() == nil {
		log.Fatal(err)
	}
}

func cmdFingerprint(args []string) {
	fs := flag.NewFlagSet("fingerprint", flag.ExitOnError)
	stateDir, _ := commonFlags(fs)
	fs.Parse(args)
	creds, err := agent.LoadCredentials(*stateDir)
	if err != nil {
		log.Fatalf("not enrolled: %v", err)
	}
	fmt.Printf("server:      %s\nfingerprint: %s\nagent id:    %s\n",
		creds.ServerURL, creds.Fingerprint, creds.AgentID)
}

// ---- job subcommands: edit agent.yaml; the daemon watches and syncs ----

type stringList []string

func (s *stringList) String() string     { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error { *s = append(*s, v); return nil }

func cmdJob(args []string) {
	if len(args) < 1 {
		usage()
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		fs := flag.NewFlagSet("job list", flag.ExitOnError)
		stateDir, config := commonFlags(fs)
		fs.Parse(rest)
		cf := mustReadConfig(resolveConfig(*stateDir, *config))
		if len(cf.Jobs) == 0 {
			fmt.Println("no jobs configured")
			return
		}
		for _, j := range cf.Jobs {
			id := j.ID
			if id == "" {
				id = "(pending sync)"
			}
			state := "enabled"
			if j.Enabled != nil && !*j.Enabled {
				state = "disabled"
			}
			sched := j.Schedule
			if sched == "" {
				sched = "manual"
			}
			fmt.Printf("%-18s %-20s %-10s %s  paths: %s\n", id, j.Name, state, sched, strings.Join(j.Paths, ", "))
		}
		if len(cf.Delete) > 0 {
			fmt.Printf("pending deletions: %s\n", strings.Join(cf.Delete, ", "))
		}

	case "add":
		fs := flag.NewFlagSet("job add", flag.ExitOnError)
		name := fs.String("name", "", "job name (required)")
		var paths, excludes stringList
		fs.Var(&paths, "path", "path to back up (repeatable, required)")
		fs.Var(&excludes, "exclude", "exclude glob (repeatable)")
		schedule := fs.String("schedule", "", "cron schedule, e.g. '0 2 * * *' (empty = manual)")
		fullEvery := fs.Int("full-every", 0, "every Nth run is a full backup (0 = always incremental)")
		keepLast := fs.Int("keep-last", 0, "retention: keep newest N snapshots (0 = all)")
		keepDays := fs.Int("keep-days", 0, "retention: also keep snapshots newer than N days")
		preHook := fs.String("pre-hook", "", "command to run before backup")
		postHook := fs.String("post-hook", "", "command to run after backup")
		disabled := fs.Bool("disabled", false, "create the job disabled")
		stateDir, config := commonFlags(fs)
		fs.Parse(rest)
		if *name == "" || len(paths) == 0 {
			fmt.Fprintln(os.Stderr, "job add: --name and at least one --path are required")
			os.Exit(2)
		}
		path := resolveConfig(*stateDir, *config)
		cf := mustReadConfig(path)
		for _, j := range cf.Jobs {
			if j.Name == *name {
				log.Fatalf("a job named %q already exists", *name)
			}
		}
		enabled := !*disabled
		catchup := true
		cf.Jobs = append(cf.Jobs, agent.YamlJob{
			Name: *name, Paths: paths, Excludes: excludes,
			Schedule: *schedule, FullEvery: *fullEvery,
			KeepLast: *keepLast, KeepDays: *keepDays,
			PreHook: *preHook, PostHook: *postHook,
			Enabled: &enabled, Catchup: &catchup,
		})
		mustWriteConfig(path, cf)
		fmt.Printf("job %q added to %s — the running agent will sync it to the server\n", *name, path)

	case "rm":
		fs := flag.NewFlagSet("job rm", flag.ExitOnError)
		stateDir, config := commonFlags(fs)
		name, flagArgs := splitPositional(rest)
		fs.Parse(flagArgs)
		if name == "" && fs.NArg() == 1 {
			name = fs.Arg(0)
		}
		if name == "" {
			fmt.Fprintln(os.Stderr, "job rm: exactly one job NAME or ID required")
			os.Exit(2)
		}
		path := resolveConfig(*stateDir, *config)
		cf := mustReadConfig(path)
		idx := findJob(cf, name)
		if idx < 0 {
			log.Fatalf("no job named or with id %q", name)
		}
		j := cf.Jobs[idx]
		cf.Jobs = append(cf.Jobs[:idx], cf.Jobs[idx+1:]...)
		if j.ID != "" {
			cf.Delete = append(cf.Delete, j.ID)
		}
		mustWriteConfig(path, cf)
		fmt.Printf("job %q removed — the running agent will delete it on the server\n", j.Name)

	case "enable", "disable":
		fs := flag.NewFlagSet("job "+sub, flag.ExitOnError)
		stateDir, config := commonFlags(fs)
		name, flagArgs := splitPositional(rest)
		fs.Parse(flagArgs)
		if name == "" && fs.NArg() == 1 {
			name = fs.Arg(0)
		}
		if name == "" {
			fmt.Fprintf(os.Stderr, "job %s: exactly one job NAME or ID required\n", sub)
			os.Exit(2)
		}
		path := resolveConfig(*stateDir, *config)
		cf := mustReadConfig(path)
		idx := findJob(cf, name)
		if idx < 0 {
			log.Fatalf("no job named or with id %q", name)
		}
		v := sub == "enable"
		cf.Jobs[idx].Enabled = &v
		mustWriteConfig(path, cf)
		fmt.Printf("job %q %sd\n", cf.Jobs[idx].Name, sub)

	default:
		usage()
	}
}

// splitPositional pulls a leading positional argument out so flags may
// appear before or after it (Go's flag package stops at the first
// non-flag argument).
func splitPositional(args []string) (positional string, flags []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func findJob(cf *agent.ConfigFile, nameOrID string) int {
	for i, j := range cf.Jobs {
		if j.Name == nameOrID || (j.ID != "" && j.ID == nameOrID) {
			return i
		}
	}
	return -1
}

func mustReadConfig(path string) *agent.ConfigFile {
	cf, err := agent.ReadConfigFile(path)
	if err != nil {
		log.Fatal(err)
	}
	return cf
}

func mustWriteConfig(path string, cf *agent.ConfigFile) {
	if err := agent.WriteConfigFile(path, cf); err != nil {
		log.Fatal(err)
	}
}
