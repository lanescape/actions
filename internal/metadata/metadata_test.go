package metadata

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestToken(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Metadata-Flavor") != "Google" || r.URL.Path != "/computeMetadata/v1/instance/service-accounts/default/identity" ||
			r.URL.Query().Get("audience") != "https://service.example?x=1" || r.URL.Query().Get("format") != "full" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("header.payload.signature\n"))
	}))
	defer srv.Close()
	tok, err := Server{URL: srv.URL}.Token(context.Background(), "https://service.example?x=1")
	if err != nil || tok != "header.payload.signature" {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if _, err := (Server{URL: srv.URL}).Token(context.Background(), ""); err == nil || strings.Contains(err.Error(), "signature") {
		t.Fatalf("a refusal: %v", err)
	}
	if _, err := (Server{URL: "http://127.0.0.1:1"}).Token(context.Background(), "aud"); err == nil {
		t.Fatal("no error from a server that isn't there")
	}
}
