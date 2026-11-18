package agent

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/identity"
	"github.com/codentient/teams-a2a/internal/testagent"
)

// trap records everything a redirect target receives.
type trap struct {
	mu   sync.Mutex
	srv  *httptest.Server
	hits []string
}

func newTrap(t *testing.T) *trap {
	tr := &trap{}
	tr.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		tr.mu.Lock()
		tr.hits = append(tr.hits, r.Header.Get("Authorization")+" "+r.Header.Get("X-User-Id")+" "+string(b))
		tr.mu.Unlock()
		http.Error(w, "x", http.StatusInternalServerError)
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}

func (tr *trap) received() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.hits...)
}

// Security review M2: a 307/308 from the pinned agent endpoint must not replay the message,
// identity headers or credentials to another origin, whatever client the caller passes in.
func TestAgentCallDoesNotFollowRedirects(t *testing.T) {
	for _, host := range []string{"localhost", "127.0.0.1"} { // other host; same host, other port
		t.Run(host, func(t *testing.T) {
			tr := newTrap(t)
			evil := strings.Replace(tr.srv.URL, "127.0.0.1", host, 1)
			ta := &testagent.Agent{}
			var legit *httptest.Server
			legit = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost {
					http.Redirect(w, r, evil+"/steal", http.StatusTemporaryRedirect)
					return
				}
				ta.Handler(legit.URL, "/agents/kagent/demo").ServeHTTP(w, r)
			}))
			defer legit.Close()
			cfg := config.Agent{
				CardURL:         legit.URL + "/agents/kagent/demo/.well-known/agent-card.json",
				IdentityHeaders: map[string]string{"X-User-Id": "aadObjectId"},
			}
			cfg.CardRefreshInterval.Duration = 1 << 40
			creds, _, _ := NewCredentials(config.AgentAuth{Type: "bearer", Token: &config.SecretRef{Value: "agent-bearer-token"}}, nil)
			ag := New(cfg, creds, http.DefaultClient) // a client that follows redirects
			ctx := identity.With(context.Background(), identity.User{ID: "alice-oid"})
			var failed bool
			for _, err := range ag.Stream(ctx, a2a.NewMessage(a2a.MessageRoleUser, a2a.NewTextPart("prod db password is hunter2"))) {
				if err != nil {
					failed = true
				}
			}
			if got := tr.received(); len(got) != 0 {
				t.Fatalf("redirect target received %q", got)
			}
			if !failed {
				t.Fatal("a redirected agent call must fail")
			}
		})
	}
}

func TestOAuth2TokenRequestDoesNotFollowRedirects(t *testing.T) {
	tr := newTrap(t)
	tokenURL := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, tr.srv.URL, http.StatusTemporaryRedirect)
	}))
	defer tokenURL.Close()
	creds, _, err := NewCredentials(config.AgentAuth{Type: "oauth2ClientCredentials", OAuth2: &config.OAuth2{
		TokenURL: tokenURL.URL, ClientID: "id", ClientSecret: &config.SecretRef{Value: "client-secret"},
	}}, http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := creds.Authorization(context.Background(), identity.User{}); err == nil {
		t.Fatal("a redirected token request must fail")
	}
	if got := tr.received(); len(got) != 0 {
		t.Fatalf("redirect target received %q", got)
	}
}
