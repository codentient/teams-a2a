// Package testbot fakes the Microsoft side of a Teams bot: the Bot Connector OpenID
// metadata and signing keys, the Entra token endpoint and the Connector REST API. It lets
// the gateway be exercised end to end without Azure.
package testbot

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	Issuer    = "https://api.botframework.com"
	BotToken  = "test-bot-token"
	KeyID     = "test-key"
	ChannelID = "msteams"
)

// Call is one request the gateway made to the fake Connector.
type Call struct {
	Method string
	Path   string
	Auth   string
	Body   map[string]any
}

func (c Call) Type() string { s, _ := c.Body["type"].(string); return s }
func (c Call) Text() string { s, _ := c.Body["text"].(string); return s }

// StreamInfo returns the streaminfo entity, if any.
func (c Call) StreamInfo() map[string]any {
	ents, _ := c.Body["entities"].([]any)
	for _, e := range ents {
		if m, ok := e.(map[string]any); ok && m["type"] == "streaminfo" {
			return m
		}
	}
	return nil
}

// Card returns the first Adaptive Card attachment content, if any.
func (c Call) Card() map[string]any {
	atts, _ := c.Body["attachments"].([]any)
	for _, a := range atts {
		if m, ok := a.(map[string]any); ok && m["contentType"] == "application/vnd.microsoft.card.adaptive" {
			content, _ := m["content"].(map[string]any)
			return content
		}
	}
	return nil
}

type Fake struct {
	Server *httptest.Server
	Key    *rsa.PrivateKey
	// Endorsements are attached to the signing key in the JWKS.
	Endorsements []string
	// ClientSecrets, when set, makes the token endpoint accept the client-credentials grant
	// only for these app id -> client secret pairs, like Entra would.
	ClientSecrets map[string]string

	mu      sync.Mutex
	calls   []Call
	nextID  int
	signal  chan struct{}
	failure map[string]int    // path substring -> status to return
	roles   map[string]string // aadObjectId -> Teams userRole (default "user")
}

func New() *Fake { return NewOn(nil) }

// NewOn serves the fake on the given listener (nil: a random local port).
func NewOn(l net.Listener) *Fake {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	f := &Fake{Key: key, Endorsements: []string{ChannelID}, signal: make(chan struct{}, 1000), failure: map[string]int{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/.well-known/openidconfiguration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": Issuer, "jwks_uri": f.Server.URL + "/v1/.well-known/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("GET /v1/.well-known/keys", func(w http.ResponseWriter, _ *http.Request) {
		pub := f.Key.PublicKey
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]any{
			"kty": "RSA", "use": "sig", "kid": KeyID,
			"n":            base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
			"e":            base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
			"endorsements": f.Endorsements,
		}}})
	})
	mux.HandleFunc("POST /{tenant}/oauth2/v2.0/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		secret, assertion := r.Form.Get("client_secret"), r.Form.Get("client_assertion")
		f.mu.Lock()
		want, known := f.ClientSecrets[r.Form.Get("client_id")]
		strict := len(f.ClientSecrets) > 0
		f.mu.Unlock()
		if (secret == "" && assertion == "") || (strict && secret != "" && (!known || secret != want)) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client","error_description":"invalid client id or secret"}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": BotToken, "expires_in": 3600, "token_type": "Bearer"})
	})
	mux.HandleFunc("/v3/conversations/", f.connector)
	f.Server = httptest.NewUnstartedServer(mux)
	if l != nil {
		_ = f.Server.Listener.Close()
		f.Server.Listener = l
	}
	f.Server.Start()
	return f
}

func (f *Fake) Close() { f.Server.Close() }

// SetClientSecret makes the token endpoint require secret for appID.
func (f *Fake) SetClientSecret(appID, secret string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ClientSecrets == nil {
		f.ClientSecrets = map[string]string{}
	}
	f.ClientSecrets[appID] = secret
}

// SetRole sets the Teams userRole the roster API reports for a user (default "user").
func (f *Fake) SetRole(aadObjectID, role string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.roles == nil {
		f.roles = map[string]string{}
	}
	f.roles[aadObjectID] = role
}

// FailPath makes Connector calls whose path contains substr return status.
func (f *Fake) FailPath(substr string, status int) {
	f.mu.Lock()
	f.failure[substr] = status
	f.mu.Unlock()
}

func (f *Fake) connector(w http.ResponseWriter, r *http.Request) {
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	f.mu.Lock()
	for sub, status := range f.failure {
		if strings.Contains(r.URL.Path, sub) && ((r.Method == http.MethodPost && body["type"] == "typing") || r.Method == http.MethodGet) {
			f.mu.Unlock()
			w.WriteHeader(status)
			return
		}
	}
	f.calls = append(f.calls, Call{Method: r.Method, Path: r.URL.EscapedPath(), Auth: r.Header.Get("Authorization"), Body: body})
	f.nextID++
	id := fmt.Sprintf("act-%d", f.nextID)
	f.mu.Unlock()
	select {
	case f.signal <- struct{}{}:
	default:
	}
	if r.Header.Get("Authorization") != "Bearer "+BotToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/members/") {
		// Members are addressed by their Teams id "29:<aadObjectId>" in the e2e tests.
		id, _ := url.PathUnescape(r.URL.EscapedPath()[strings.LastIndex(r.URL.EscapedPath(), "/")+1:])
		aad := strings.TrimPrefix(id, "29:")
		f.mu.Lock()
		role := f.roles[aad]
		f.mu.Unlock()
		if role == "" {
			role = "user"
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id, "aadObjectId": aad, "userRole": role})
		return
	}
	if r.Method == http.MethodPut {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"`+id+`"}`)
		return
	}
	// The first streaming activity returns 201 with the stream id, later ones 202.
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
}

func (f *Fake) Calls() []Call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Call(nil), f.calls...)
}

func (f *Fake) Reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

// WaitFor polls until pred matches a call (returning it) or the timeout expires.
func (f *Fake) WaitFor(timeout time.Duration, pred func(Call) bool) (Call, bool) {
	deadline := time.After(timeout)
	for {
		for _, c := range f.Calls() {
			if pred(c) {
				return c, true
			}
		}
		select {
		case <-deadline:
			return Call{}, false
		case <-f.signal:
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// Token mints a Bot Connector -> bot token. mutate can tamper with claims for negative tests.
func (f *Fake) Token(appID, serviceURL string, mutate func(jwt.MapClaims)) string {
	now := time.Now()
	claims := jwt.MapClaims{
		"iss": Issuer, "aud": appID, "serviceurl": serviceURL,
		"nbf": now.Add(-time.Minute).Unix(), "exp": now.Add(time.Hour).Unix(),
	}
	if mutate != nil {
		mutate(claims)
	}
	t := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	t.Header["kid"] = KeyID
	s, err := t.SignedString(f.Key)
	if err != nil {
		panic(err)
	}
	return s
}
