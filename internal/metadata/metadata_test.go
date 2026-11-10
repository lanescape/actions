package metadata

import (
	"context"
	"testing"

	"github.com/lanescape/runners/emulators/service"
)

// The token comes from the machine's metadata server, as lanescape/runners'
// emulator of the service serves its machines one, for the audience asked
// for, in the full format.
func TestToken(t *testing.T) {
	svc := service.New()
	defer svc.Close()
	repo := svc.Repository("app")
	j, err := svc.StartJob(svc.Push(repo, "main"), "lanescape-2vcpu-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := Server{URL: svc.Metadata(j)}.Token(context.Background(), svc.URL())
	if err != nil || tok == "" || tok != svc.Token(j) && len(tok) < 100 {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if _, err := (Server{URL: svc.Metadata(j)}).Token(context.Background(), ""); err == nil {
		t.Fatal("a token with no audience")
	}
	if _, err := (Server{URL: "http://127.0.0.1:1"}).Token(context.Background(), "aud"); err == nil {
		t.Fatal("no error from a server that isn't there")
	}
}
