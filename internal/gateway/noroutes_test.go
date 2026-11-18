package gateway_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/gateway"
	"github.com/codentient/teams-a2a/internal/store"
	"github.com/codentient/teams-a2a/internal/testbot"
)

// A gateway can be deployed before any bot exists: it becomes ready and refuses every
// Bot Service request, even a correctly signed one.
func TestGatewayWithoutRoutesRejectsEverything(t *testing.T) {
	ms := testbot.New()
	t.Cleanup(ms.Close)
	cfg, err := config.Parse([]byte(`
teams:
  openIdMetadataURL: ` + ms.Server.URL + `/v1/.well-known/openidconfiguration
  loginAuthority: ` + ms.Server.URL + `
  serviceUrlAllowlist: ["127.0.0.1"]
  allowInsecureServiceURLs: true
`))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := gateway.New(cfg, gateway.Deps{
		Store:      store.NewMemory(time.Hour, 10),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		HTTPClient: http.DefaultClient,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := srv.Warmup(context.Background()); err != nil {
		t.Fatal(err)
	}
	srv.SetReady()
	admin := httptest.NewServer(srv.AdminHandler())
	t.Cleanup(admin.Close)
	gw := httptest.NewServer(srv.Handler())
	t.Cleanup(gw.Close)

	resp, err := http.Get(admin.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/readyz = %d, want 200", resp.StatusCode)
	}

	serviceURL := ms.Server.URL + "/"
	body, _ := json.Marshal(map[string]any{
		"type": "message", "id": "1", "channelId": "msteams", "serviceUrl": serviceURL, "text": "hi",
		"from":         map[string]any{"id": "29:" + alice, "aadObjectId": alice},
		"recipient":    map[string]any{"id": "28:" + appID},
		"conversation": map[string]any{"id": "a:1", "conversationType": "personal", "tenantId": tenantID},
	})
	req, _ := http.NewRequest(http.MethodPost, gw.URL+"/api/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+ms.Token(appID, serviceURL, nil))
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("signed request to a gateway without routes = %d, want 401", resp.StatusCode)
	}
	if calls := ms.Calls(); len(calls) != 0 {
		t.Fatalf("no reply may be sent without a route, got %d connector calls", len(calls))
	}
}
