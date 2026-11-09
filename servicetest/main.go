// Command servicetest runs the runner service, lanescape/runners at the
// version go.mod names, against that repository's own emulators, for the
// actions' end-to-end tests (specs/0002-releases.md, Tests). It is not part
// of any action, and no action relies on it.
//
// The service's command reaches only GitHub and Google themselves, and its
// packages are internal to its module, so this program builds it from
// those packages, as that repository's own end-to-end tests do. Only a
// module whose path is inside github.com/lanescape/runners may import
// them, hence this module's path. It serves the interface for actions:
// POST /v1/bind, checking the job's OIDC token against the GitHub
// emulator's issuer, and the run against its API; its state bucket is a
// Cloud Storage emulator of its own. It stands in until lanescape/runners
// offers a way of its own to run the service against its emulators; then
// the tests use that instead.
//
//	servicetest -github <url> -app-id <id> -app-key <file> \
//	  -issuer <url> -jwks <url> -address-file <file>
//
// It writes its address, which is also its tokens' audience, to the
// address file once it listens, and serves until it's killed.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/jonboulle/clockwork"

	"github.com/lanescape/runners/emulators/google"
	gcsemu "github.com/lanescape/runners/emulators/storage"
	"github.com/lanescape/runners/endpoints"
	"github.com/lanescape/runners/internal/auth"
	"github.com/lanescape/runners/internal/binding"
	"github.com/lanescape/runners/internal/github"
	"github.com/lanescape/runners/internal/lease"
	"github.com/lanescape/runners/internal/pass"
	"github.com/lanescape/runners/internal/server"
	"github.com/lanescape/runners/internal/store"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "servicetest:", err)
		os.Exit(1)
	}
}

func run() error {
	ghURL := flag.String("github", "", "the GitHub emulator's API")
	appID := flag.Int64("app-id", 0, "the service's GitHub App's ID")
	appKey := flag.String("app-key", "", "a file holding the App's PEM private key")
	issuer := flag.String("issuer", "", "the GitHub emulator's Actions OIDC issuer")
	jwks := flag.String("jwks", "", "the issuer's key set")
	addressFile := flag.String("address-file", "", "where to write the service's address once it listens")
	flag.Parse()
	if *ghURL == "" || *appID <= 0 || *appKey == "" || *issuer == "" || *jwks == "" || *addressFile == "" {
		flag.Usage()
		return errors.New("every flag is required")
	}
	key, err := os.ReadFile(*appKey)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))

	gcs := gcsemu.New()
	defer gcs.Close()
	gcs.CreateBucket("state")
	objects, err := store.NewGCS(ctx, "state", gcs.ClientOptions()...)
	if err != nil {
		return err
	}
	defer objects.Close()
	gg := google.New()
	defer gg.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	audience := "http://" + ln.Addr().String()

	gh, err := github.NewAppClient(github.Options{BaseURL: *ghURL, AppID: *appID, PrivateKey: key, OrganizationID: endpoints.Organization})
	if err != nil {
		return err
	}
	callers, err := auth.NewGoogle(auth.Options{Issuer: auth.Issuer{URL: gg.URL, JWKSURL: gg.JWKSURL}, Audience: audience},
		[]string{"scheduler@lanescape-dev.iam.gserviceaccount.com"})
	if err != nil {
		return err
	}
	actions, err := auth.NewActions(auth.Options{Issuer: auth.Issuer{URL: *issuer, JWKSURL: *jwks}, Audience: audience}, endpoints.Organization)
	if err != nil {
		return err
	}
	clock := clockwork.NewRealClock()
	binder := &binding.Binder{GitHub: gh, Objects: objects, OrganizationID: endpoints.Organization, Clock: clock, Logger: log}
	passes := &pass.Runner{Lease: lease.New(objects, pass.LeaseName, pass.Validity, clock), Clock: clock, Logger: log}
	h, err := server.New(server.Config{
		Logger:           log,
		OrganizationID:   endpoints.Organization,
		WebhookSecret:    []byte("unused"),
		Publisher:        noPublisher{},
		ReconcileCallers: callers,
		Passes:           passes,
		Reconciler:       noReconciler{},
		Actions:          actions,
		Binder:           binder,
	})
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: h}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if err := os.WriteFile(*addressFile, []byte(audience+"\n"), 0o644); err != nil {
		return err
	}
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		return srv.Close()
	}
}

// The tests use only the interface for actions: no webhooks, and no
// passes, which make machines.
type noPublisher struct{}

func (noPublisher) Publish(context.Context, server.Delivery) error {
	return errors.New("no Pub/Sub here")
}

type noReconciler struct{}

func (noReconciler) Pass(*pass.Pass) error { return nil }
