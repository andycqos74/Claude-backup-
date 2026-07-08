// backup-server is the central backup server: web GUI, admin API, agent
// control channel and backup storage. Configuration comes from CB_*
// environment variables (see internal/server.ConfigFromEnv).
package main

import (
	"log"

	"centralbackup/internal/server"
)

func main() {
	srv, err := server.New(server.ConfigFromEnv())
	if err != nil {
		log.Fatalf("startup: %v", err)
	}
	log.Fatal(srv.Run())
}
