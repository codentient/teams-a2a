package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/httpx"
	"github.com/codentient/teams-a2a/internal/identity"
)

// Credentials produce the Authorization header value for a call made on behalf of a user.
type Credentials interface {
	Authorization(ctx context.Context, user identity.User) (string, error)
}

type noCredentials struct{}

func (noCredentials) Authorization(context.Context, identity.User) (string, error) { return "", nil }

type staticBearer struct{ ref *config.SecretRef }

func (s staticBearer) Authorization(context.Context, identity.User) (string, error) {
	v, err := s.ref.Resolve()
	if err != nil {
		return "", err
	}
	return "Bearer " + v, nil
}

// fileBearer re-reads a token file (e.g. a projected Kubernetes ServiceAccount token, which
// the kubelet rotates) at most once a minute.
type fileBearer struct {
	path string
	mu   sync.Mutex
	val  string
	read time.Time
}

func (f *fileBearer) Authorization(context.Context, identity.User) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.val == "" || time.Since(f.read) > time.Minute {
		b, err := os.ReadFile(f.path)
		if err != nil {
			if f.val != "" {
				return "Bearer " + f.val, nil
			}
			return "", fmt.Errorf("read agent token file: %w", err)
		}
		f.val, f.read = strings.TrimSpace(string(b)), time.Now()
	}
	return "Bearer " + f.val, nil
}

type clientCredentials struct {
	cfg *config.OAuth2
	hc  *http.Client

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func (c *clientCredentials) Authorization(ctx context.Context, _ identity.User) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expiry) > time.Minute {
		return "Bearer " + c.token, nil
	}
	secret, err := c.cfg.ClientSecret.Resolve()
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {c.cfg.ClientID},
		"client_secret": {secret},
	}
	if len(c.cfg.Scopes) > 0 {
		form.Set("scope", strings.Join(c.cfg.Scopes, " "))
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.hc.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("agent token endpoint status %d %s", resp.StatusCode, tr.Error)
	}
	if tr.ExpiresIn <= 0 {
		tr.ExpiresIn = 300
	}
	c.token, c.expiry = tr.AccessToken, time.Now().Add(time.Duration(tr.ExpiresIn)*time.Second)
	return "Bearer " + c.token, nil
}

// UserJWTSigner mints short-lived ES256 JWTs that assert which Teams user a call is for.
// It is meant to sit behind a proxy that validates the token against the gateway's JWKS
// (e.g. agentgateway in front of kagent running with config.server.auth.mode=trusted-proxy).
type UserJWTSigner struct {
	cfg *config.UserJWT
	key *ecdsa.PrivateKey
	kid string
	now func() time.Time

	mu    sync.Mutex
	cache map[string]cachedJWT
}

type cachedJWT struct {
	token  string
	expiry time.Time
}

func NewUserJWTSigner(cfg *config.UserJWT) (*UserJWTSigner, error) {
	b, err := os.ReadFile(cfg.SigningKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read user JWT signing key: %w", err)
	}
	block, _ := pem.Decode(b)
	if block == nil {
		return nil, errors.New("user JWT signing key is not PEM")
	}
	var key *ecdsa.PrivateKey
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		ek, ok := k.(*ecdsa.PrivateKey)
		if !ok {
			return nil, errors.New("user JWT signing key must be an EC P-256 key")
		}
		key = ek
	} else if ek, err2 := x509.ParseECPrivateKey(block.Bytes); err2 == nil {
		key = ek
	} else {
		return nil, fmt.Errorf("parse user JWT signing key: %w", err)
	}
	if key.Curve != elliptic.P256() {
		return nil, errors.New("user JWT signing key must use curve P-256 (ES256)")
	}
	return &UserJWTSigner{cfg: cfg, key: key, kid: thumbprint(&key.PublicKey), now: time.Now, cache: map[string]cachedJWT{}}, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func pad32(b []byte) []byte {
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// thumbprint is the RFC 7638 JWK thumbprint, used as the key id.
func thumbprint(pub *ecdsa.PublicKey) string {
	j := fmt.Sprintf(`{"crv":"P-256","kty":"EC","x":"%s","y":"%s"}`, b64(pad32(pub.X.Bytes())), b64(pad32(pub.Y.Bytes())))
	sum := sha256.Sum256([]byte(j))
	return b64(sum[:])
}

// JWKS returns the public key set to publish for validators.
func (s *UserJWTSigner) JWKS() map[string]any {
	pub := s.key.PublicKey
	return map[string]any{"keys": []map[string]any{{
		"kty": "EC", "crv": "P-256", "use": "sig", "alg": "ES256", "kid": s.kid,
		"x": b64(pad32(pub.X.Bytes())), "y": b64(pad32(pub.Y.Bytes())),
	}}}
}

func (s *UserJWTSigner) Authorization(_ context.Context, user identity.User) (string, error) {
	sub := user.Field(s.cfg.Subject)
	if sub == "" {
		return "", fmt.Errorf("user has no %s; refusing to mint an anonymous token", s.cfg.Subject)
	}
	cacheKey := sub + "|" + user.TenantID + "|" + user.Name
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.cache[cacheKey]; ok && c.expiry.Sub(now) > s.cfg.TTL.Duration/3 {
		return "Bearer " + c.token, nil
	}
	jti := make([]byte, 16)
	_, _ = rand.Read(jti)
	exp := now.Add(s.cfg.TTL.Duration)
	claims := jwt.MapClaims{
		"iss":  s.cfg.Issuer,
		"aud":  s.cfg.Audience,
		"sub":  sub,
		"tid":  user.TenantID,
		"oid":  user.ID,
		"name": user.Name,
		"iat":  now.Unix(),
		"nbf":  now.Add(-30 * time.Second).Unix(),
		"exp":  exp.Unix(),
		"jti":  hex.EncodeToString(jti),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, claims)
	tok.Header["kid"] = s.kid
	signed, err := tok.SignedString(s.key)
	if err != nil {
		return "", err
	}
	if len(s.cache) > 10_000 {
		clear(s.cache)
	}
	s.cache[cacheKey] = cachedJWT{token: signed, expiry: exp}
	return "Bearer " + signed, nil
}

// NewCredentials builds the agent credential for a route.
func NewCredentials(cfg config.AgentAuth, hc *http.Client) (Credentials, *UserJWTSigner, error) {
	hc = httpx.NoRedirects(hc)
	switch cfg.Type {
	case "none":
		return noCredentials{}, nil, nil
	case "bearer":
		return staticBearer{ref: cfg.Token}, nil, nil
	case "tokenFile":
		return &fileBearer{path: cfg.TokenFile}, nil, nil
	case "oauth2ClientCredentials":
		return &clientCredentials{cfg: cfg.OAuth2, hc: hc}, nil, nil
	case "userJWT":
		s, err := NewUserJWTSigner(cfg.UserJWT)
		if err != nil {
			return nil, nil, err
		}
		return s, s, nil
	}
	return nil, nil, fmt.Errorf("unsupported agent auth type %q", cfg.Type)
}
