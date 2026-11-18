package gateway_test

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/gateway"
	"github.com/codentient/teams-a2a/internal/store"
)

func secretFile(t *testing.T, v string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "clientSecret")
	if err := os.WriteFile(f, []byte(v+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	return f
}

func botWithSecretFile(f string) string {
	return `
      appId: ` + appID + `
      tenantId: ` + tenantID + `
      auth: {type: clientSecret, clientSecret: {file: ` + f + `}}`
}

func TestTeamsClientSecretFromMountedFile(t *testing.T) {
	e := newEnv(t, opts{bot: botWithSecretFile(secretFile(t, "s3cret"))})
	e.send(e.activity("hello from a secret"))
	e.waitFinal("Echo: hello from a secret")
}

func TestWrongTeamsSecretIsRejectedByTokenEndpoint(t *testing.T) {
	e := newEnv(t, opts{bot: botWithSecretFile(secretFile(t, "wrong"))})
	e.send(e.activity("hello"))
	time.Sleep(time.Second)
	for _, c := range e.ms.Calls() {
		if c.Type() == "message" {
			t.Fatalf("a reply must not be delivered with a rejected secret: %s", dump(e.ms.Calls()))
		}
	}
}

func TestMissingTeamsSecretFailsAtStartup(t *testing.T) {
	cfg, err := config.Parse([]byte(`
routes:
  - name: demo
    bot: ` + botWithSecretFile("/nonexistent/teams/clientSecret") + `
    allowedTenants: [` + tenantID + `]
    allowAllTenantUsers: true
    agent: {cardURL: http://127.0.0.1:1/}
`))
	if err != nil {
		t.Fatal(err)
	}
	_, err = gateway.New(cfg, gateway.Deps{
		Store:      store.NewMemory(time.Hour, 10),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPClient: http.DefaultClient,
	})
	if err == nil || !strings.Contains(err.Error(), "clientSecret") {
		t.Fatalf("want a startup error about the client secret, got %v", err)
	}
}
