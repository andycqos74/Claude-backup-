package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"centralbackup/internal/agent"
	"centralbackup/internal/bundle"
)

// `backup-agent install` is the whole client setup in one step: enroll if
// needed, then register the background service. It takes its details from
// flags, or from an enrollment the server appended to the binary — in which
// case the entire installation is "download this file and run it", with
// nothing to paste and no bootstrap script to go wrong.

func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	server := fs.String("server", "", "server URL, e.g. https://backup.example.com:8443")
	token := fs.String("token", "", "one-time enrollment token from the server GUI")
	fingerprint := fs.String("fingerprint", "", "expected server TLS certificate SHA-256 fingerprint")
	name := fs.String("name", "", "display name for this client (default: hostname)")
	stateDir, configPath := commonFlags(fs)
	fs.Parse(args)

	// Flags win over the embedded values, so a bundled binary can still be
	// pointed somewhere else without rebuilding it.
	if e, ok, err := bundle.ReadSelf(); err != nil {
		log.Printf("warning: could not read embedded enrollment: %v", err)
	} else if ok {
		if *server == "" {
			*server = e.ServerURL
		}
		if *token == "" {
			*token = e.Token
		}
		if *fingerprint == "" {
			*fingerprint = e.Fingerprint
		}
		if *name == "" {
			*name = e.Name
		}
	}

	if _, err := agent.LoadCredentials(*stateDir); err == nil {
		fmt.Println("already enrolled; installing the service only")
	} else {
		if *server == "" || *token == "" {
			fmt.Fprintln(os.Stderr,
				"install: no enrollment details.\n"+
					"Either download the ready-to-run installer from the server's Clients page,\n"+
					"or pass --server and --token from Clients → Enroll new client.")
			os.Exit(2)
		}
		if err := enroll(strings.TrimRight(*server, "/"), *token, *fingerprint, *name, *stateDir); err != nil {
			log.Fatalf("enrollment failed: %v", err)
		}
	}

	if err := installService(*stateDir, configOrDefault(configPath, *stateDir)); err != nil {
		log.Fatalf("could not install the service: %v", err)
	}
}

func cmdUninstall(args []string) {
	fs := flag.NewFlagSet("uninstall", flag.ExitOnError)
	purge := fs.Bool("purge", false, "also delete this client's credentials and local job config")
	stateDir, _ := commonFlags(fs)
	fs.Parse(args)

	if err := uninstallService(); err != nil {
		log.Fatal(err)
	}
	if *purge {
		if err := os.RemoveAll(*stateDir); err != nil {
			log.Fatalf("could not remove %s: %v", *stateDir, err)
		}
		fmt.Println("credentials and local job config removed")
		fmt.Println("note: the client still exists on the server — delete it there too")
	}
}

func configOrDefault(configPath *string, stateDir string) string {
	if configPath != nil && *configPath != "" {
		return *configPath
	}
	return defaultConfigPath(stateDir)
}
