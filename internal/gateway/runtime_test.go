package gateway_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codentient/teams-a2a/internal/config"
)

// A route can allow a slow (synchronous) agent more time before the first response byte, and
// the timeout still applies.
func TestAgentResponseHeaderTimeoutIsPerRoute(t *testing.T) {
	for _, tc := range []struct {
		name    string
		timeout time.Duration
		want    string
	}{
		{"too short", time.Second, "couldn't get an answer"},
		{"long enough", 5 * time.Second, "Echo: slow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(t, opts{mutate: func(c *config.Config) {
				c.Routes[0].Agent.ResponseHeaderTimeout.Duration = tc.timeout
				c.Routes[0].Streaming = new(bool) // non-streaming SendMessage
			}})
			inner := e.agentTS.Config.Handler
			e.agentTS.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					time.Sleep(2 * time.Second) // the agent thinks before answering at all
				}
				inner.ServeHTTP(w, r)
			})
			e.send(e.activity("slow"))
			e.waitFinal(tc.want)
		})
	}
}

// On shutdown the gateway reports not-ready first, so the endpoint is removed before the
// listener closes.
func TestSetNotReadyFailsReadiness(t *testing.T) {
	e := newEnv(t, opts{})
	admin := httptest.NewServer(e.srv.AdminHandler())
	defer admin.Close()
	get := func() int {
		resp, err := http.Get(admin.URL + "/readyz")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if get() != http.StatusOK {
		t.Fatal("should be ready")
	}
	e.srv.SetNotReady()
	if got := get(); got != http.StatusServiceUnavailable {
		t.Fatalf("/readyz after SetNotReady = %d, want 503", got)
	}
}
