package gateway_test

import (
	"bytes"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/codentient/teams-a2a/internal/config"
)

// Security review L8: requests without a bearer token are refused before the body is parsed,
// and rejection logs are sampled.
func TestRequestsWithoutBearerAreRefusedBeforeParsing(t *testing.T) {
	e := newEnv(t, opts{})
	for _, auth := range []string{"", "Basic Zm9vOmJhcg==", "Bearer "} {
		req, _ := http.NewRequest(http.MethodPost, e.gw.URL+"/api/messages", strings.NewReader("{not json"))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q with a malformed body: status %d, want 401 (refused before parsing)", auth, resp.StatusCode)
		}
	}
}

func TestRejectionLogsAreSampled(t *testing.T) {
	var buf safeBuffer
	e := newEnv(t, opts{logger: slog.New(slog.NewTextHandler(&buf, nil)), mutate: func(*config.Config) {}})
	for range 100 {
		req, _ := http.NewRequest(http.MethodPost, e.gw.URL+"/api/messages", strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	if n := strings.Count(buf.String(), "rejected inbound request"); n == 0 || n > 10 {
		t.Fatalf("100 rejected requests produced %d log lines; want a few (sampled)", n)
	}
}

type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *safeBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }
