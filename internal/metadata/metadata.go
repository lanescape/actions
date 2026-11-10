// Package metadata gets the machine's identity token from its metadata
// server, as Compute Engine serves it: for an audience, in the full
// format, which names the machine. A token is a credential, and never
// logged.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Server is a machine's metadata server.
type Server struct {
	URL  string
	HTTP *http.Client
}

// Token is the machine's identity token for audience, got afresh.
func (s Server) Token(ctx context.Context, audience string) (string, error) {
	q := url.Values{"audience": {audience}, "format": {"full"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(s.URL, "/")+"/computeMetadata/v1/instance/service-accounts/default/identity?"+q.Encode(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Metadata-Flavor", "Google")
	hc := s.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", errors.New("the machine's metadata server couldn't be reached")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil || resp.StatusCode != http.StatusOK || len(b) == 0 {
		return "", fmt.Errorf("the machine's metadata server answered %d", resp.StatusCode)
	}
	return strings.TrimSpace(string(b)), nil
}
