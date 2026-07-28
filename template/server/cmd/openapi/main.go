// Command openapi writes the OpenAPI spec generated from the Go handlers to a
// file. This is the source the web client's TypeScript types are generated
// from, keeping server and client in sync.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"

	"github.com/robert-crandall-org/go-home-server/auth"
	"github.com/robert-crandall-org/go-home-server/notify"
	"github.com/robert-crandall-org/go-home-server/server"

	"github.com/robert-crandall-org/example-app/internal/notes"
)

func main() {
	out := flag.String("o", "openapi.json", "output path for the OpenAPI spec")
	flag.Parse()

	// Services are constructed with a nil pool: registering operations only
	// wires the huma API and never touches the database.
	authSvc := auth.NewService(nil, false)
	notifySvc, err := notify.NewService(nil, notify.VAPID{})
	if err != nil {
		log.Fatalf("notify: %v", err)
	}
	notesSvc := notes.NewService(nil, notifySvc)

	srv := server.New(server.Options{Title: "Example App", Version: "1.0.0"})
	authSvc.Register(srv.API)
	authSvc.RegisterTokens(srv.API)
	notify.Register(srv.API, notifySvc, func(context.Context) (int64, error) { return 0, nil })
	notesSvc.Register(srv.API)

	spec, err := json.MarshalIndent(srv.API.OpenAPI(), "", "  ")
	if err != nil {
		log.Fatalf("marshal spec: %v", err)
	}
	spec = append(spec, '\n')
	if err := os.WriteFile(*out, spec, 0o644); err != nil {
		log.Fatalf("write %s: %v", *out, err)
	}
	log.Printf("wrote %s", *out)
}
