package teams

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/codentient/teams-a2a/internal/config"
)

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
		tr.hits = append(tr.hits, r.Method+" "+r.Header.Get("Authorization")+" "+string(b))
		tr.mu.Unlock()
		_, _ = w.Write([]byte(`{"keys":[],"jwks_uri":"x","access_token":"stolen","expires_in":3600,"id":"1"}`))
	}))
	t.Cleanup(tr.srv.Close)
	return tr
}

func (tr *trap) received() []string {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	return append([]string(nil), tr.hits...)
}

func redirectTo(t *testing.T, target string) *httptest.Server {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.Path, http.StatusPermanentRedirect)
	}))
	t.Cleanup(s.Close)
	return s
}

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// Security review M2: no outbound call that carries a credential or trust decision may follow
// a redirect, whatever client the caller passes in.
func TestConnectorDoesNotFollowRedirects(t *testing.T) {
	tr := newTrap(t)
	origin := redirectTo(t, tr.srv.URL)
	c := NewConnector(staticToken("bot-token"), ServiceURLPolicy{Allowlist: []string{"127.0.0.1"}, AllowInsecure: true}, http.DefaultClient)
	if _, err := c.SendToConversation(context.Background(), origin.URL+"/", "conv", MessageActivity("secret answer")); err == nil {
		t.Fatal("a redirected Connector call must fail")
	}
	if got := tr.received(); len(got) != 0 {
		t.Fatalf("redirect target received %q", got)
	}
}

func TestBotTokenRequestDoesNotFollowRedirects(t *testing.T) {
	tr := newTrap(t)
	authority := redirectTo(t, tr.srv.URL)
	ts, err := NewTokenSource(config.Teams{LoginAuthority: authority.URL, ConnectorScope: "s"},
		config.Bot{AppID: "app", TenantID: "tenant", Auth: config.BotAuth{Type: "clientSecret", ClientSecret: &config.SecretRef{Value: "bot-secret"}}},
		http.DefaultClient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.Token(context.Background()); err == nil {
		t.Fatal("a redirected token request must fail")
	}
	if got := tr.received(); len(got) != 0 {
		t.Fatalf("redirect target received %q", got)
	}
}

func TestSigningKeysAreNotFetchedThroughRedirects(t *testing.T) {
	tr := newTrap(t)
	metadata := redirectTo(t, tr.srv.URL)
	v := NewValidator(metadata.URL+"/v1/.well-known/openidconfiguration", "https://api.botframework.com", []string{"app"}, []string{"msteams"}, http.DefaultClient)
	if err := v.Refresh(context.Background()); err == nil {
		t.Fatal("signing-key metadata served through a redirect must be refused")
	}
	if got := tr.received(); len(got) != 0 {
		t.Fatalf("redirect target was asked for trust anchors: %q", got)
	}
}
