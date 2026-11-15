package teams

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codentient/teams-a2a/internal/config"
)

func TestClientSecretFileRotatesWithoutRestart(t *testing.T) {
	var accepted atomic.Value
	accepted.Store("first")
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("client_id") != "app-1" || r.Form.Get("client_secret") != accepted.Load().(string) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"invalid_client"}`))
			return
		}
		_, _ = w.Write([]byte(`{"access_token":"tok-` + r.Form.Get("client_secret") + `","expires_in":3600}`))
	}))
	defer idp.Close()

	file := filepath.Join(t.TempDir(), "clientSecret")
	write := func(v string) {
		if err := os.WriteFile(file, []byte(v+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("first")
	bot := config.Bot{AppID: "app-1", TenantID: "tenant-1", Auth: config.BotAuth{
		Type: "clientSecret", ClientSecret: &config.SecretRef{File: file}}}
	ts, err := NewTokenSource(config.Teams{LoginAuthority: idp.URL, ConnectorScope: "s"}, bot, idp.Client())
	if err != nil {
		t.Fatal(err)
	}
	if tok, err := ts.Token(context.Background()); err != nil || tok != "tok-first" {
		t.Fatalf("first token: %q, %v", tok, err)
	}

	// The Secret is rotated in Entra and in Kubernetes; the cached token then expires.
	accepted.Store("second")
	write("second")
	ts.(*cachingSource).expiry = time.Now()
	if tok, err := ts.Token(context.Background()); err != nil || tok != "tok-second" {
		t.Fatalf("token after rotation: %q, %v", tok, err)
	}
}

func TestMissingClientSecretFailsFast(t *testing.T) {
	bot := config.Bot{AppID: "app-1", TenantID: "tenant-1", Auth: config.BotAuth{
		Type: "clientSecret", ClientSecret: &config.SecretRef{Env: "TEAMS_A2A_UNSET_SECRET"}}}
	_, err := NewTokenSource(config.Teams{LoginAuthority: "https://login.example"}, bot, http.DefaultClient)
	if err == nil || !strings.Contains(err.Error(), "TEAMS_A2A_UNSET_SECRET") {
		t.Fatalf("want error naming the missing env var, got %v", err)
	}
}
