package teams

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type keyServer struct {
	srv     *httptest.Server
	fetches atomic.Int32
	fail    atomic.Bool
}

func newKeyServer(t *testing.T) *keyServer {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	ks := &keyServer{}
	ks.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ks.fetches.Add(1)
		if ks.fail.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if strings.Contains(r.URL.Path, "openid") {
			_ = json.NewEncoder(w).Encode(map[string]string{"jwks_uri": ks.srv.URL + "/keys"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "kid": "k1", "endorsements": []string{"msteams"},
			"n": base64.RawURLEncoding.EncodeToString(key.N.Bytes()), "e": "AQAB"}}})
	}))
	t.Cleanup(ks.srv.Close)
	return ks
}

func unsignedToken(kid string) string {
	enc := base64.RawURLEncoding.EncodeToString
	return "Bearer " + enc([]byte(`{"alg":"RS256","kid":"`+kid+`"}`)) + "." + enc([]byte(`{}`)) + "." + enc([]byte("not-a-signature"))
}

// Security review M5: unauthenticated requests must never drive outbound key fetches.
func TestStaleKeysDoNotCauseFetchesPerRequest(t *testing.T) {
	ks := newKeyServer(t)
	now := time.Now()
	v := NewValidator(ks.srv.URL+"/openid", "https://api.botframework.com", []string{"app"}, []string{"msteams"}, ks.srv.Client())
	v.now = func() time.Time { return now }
	if err := v.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ks.fail.Store(true)
	now = now.Add(13 * time.Hour) // keys are stale, the metadata endpoint is down
	ks.fetches.Store(0)
	act := &Activity{ServiceURL: "https://x", ChannelID: "msteams"}
	for range 50 {
		_, _ = v.Validate(context.Background(), unsignedToken("k1"), act)
	}
	if n := ks.fetches.Load(); n != 0 {
		t.Fatalf("50 unauthenticated requests caused %d key fetches; known keys must be refreshed in the background", n)
	}
}

func TestUnknownKeyIDsFetchAtMostOncePerCooldown(t *testing.T) {
	ks := newKeyServer(t)
	now := time.Now()
	v := NewValidator(ks.srv.URL+"/openid", "https://api.botframework.com", []string{"app"}, []string{"msteams"}, ks.srv.Client())
	v.now = func() time.Time { return now }
	if err := v.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	ks.fetches.Store(0)
	act := &Activity{ServiceURL: "https://x", ChannelID: "msteams"}
	for i := range 50 {
		_, _ = v.Validate(context.Background(), unsignedToken("unknown-"+string(rune('a'+i%26))), act)
	}
	if n := ks.fetches.Load(); n > 2 { // one refresh = metadata + keys
		t.Fatalf("unknown key ids caused %d fetches within the cooldown", n)
	}
}

func TestBackgroundRefreshKeepsKeysFreshAndRetries(t *testing.T) {
	ks := newKeyServer(t)
	v := NewValidator(ks.srv.URL+"/openid", "https://api.botframework.com", []string{"app"}, []string{"msteams"}, ks.srv.Client())
	if err := v.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	first := v.fetchedAt
	ks.fail.Store(true)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go v.refreshLoop(ctx, 20*time.Millisecond, 10*time.Millisecond)
	time.Sleep(120 * time.Millisecond)
	if n := ks.fetches.Load(); n < 4 {
		t.Fatalf("the background loop should keep retrying while the endpoint fails, fetches=%d", n)
	}
	ks.fail.Store(false)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		v.mu.RLock()
		updated := v.fetchedAt.After(first)
		v.mu.RUnlock()
		if updated {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the background loop did not refresh the keys after the endpoint recovered")
}
