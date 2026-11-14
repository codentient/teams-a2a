package agent

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/identity"
)

func writeKey(t *testing.T) string {
	t.Helper()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	der, _ := x509.MarshalPKCS8PrivateKey(k)
	p := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestUserJWTIsVerifiableWithPublishedJWKS(t *testing.T) {
	cfg := &config.UserJWT{
		Issuer: "https://teams-a2a.example", Audience: []string{"kagent"},
		SigningKeyFile: writeKey(t), TTL: config.Duration{Duration: 5 * time.Minute}, Subject: "aadObjectId",
	}
	s, err := NewUserJWTSigner(cfg)
	if err != nil {
		t.Fatal(err)
	}
	user := identity.User{ID: "oid-1", Name: "Alice", TenantID: "tid-1", ChannelUserID: "29:x"}
	authz, err := s.Authorization(context.Background(), user)
	if err != nil {
		t.Fatal(err)
	}
	raw := strings.TrimPrefix(authz, "Bearer ")

	// Verify exactly as a downstream validator would: from the JWKS, not the private key.
	jwk := s.JWKS()["keys"].([]map[string]any)[0]
	dec := func(s string) *big.Int {
		b, _ := base64.RawURLEncoding.DecodeString(s)
		return new(big.Int).SetBytes(b)
	}
	pub := &ecdsa.PublicKey{Curve: elliptic.P256(), X: dec(jwk["x"].(string)), Y: dec(jwk["y"].(string))}
	tok, err := jwt.Parse(raw, func(t *jwt.Token) (any, error) {
		if t.Header["kid"] != jwk["kid"] {
			t.Valid = false
		}
		return pub, nil
	}, jwt.WithValidMethods([]string{"ES256"}), jwt.WithIssuer(cfg.Issuer), jwt.WithAudience("kagent"), jwt.WithExpirationRequired())
	if err != nil || !tok.Valid {
		t.Fatalf("token does not verify: %v", err)
	}
	claims := tok.Claims.(jwt.MapClaims)
	if claims["sub"] != "oid-1" || claims["tid"] != "tid-1" {
		t.Errorf("claims: %v", claims)
	}

	again, _ := s.Authorization(context.Background(), user)
	if again != authz {
		t.Error("token should be cached while fresh")
	}
	if _, err := s.Authorization(context.Background(), identity.User{}); err == nil {
		t.Error("must refuse to mint a token without a subject")
	}
}

func TestPinInterfaces(t *testing.T) {
	a := New(config.Agent{CardURL: "https://agents.example.com/agents/ns/a/.well-known/agent-card.json"}, noCredentials{}, nil)
	card := cardWith("https://agents.example.com:443/agents/ns/a", "https://elsewhere.example.com/a2a", "http://agents.example.com/agents/ns/a")
	pinned, err := a.pinInterfaces(card)
	if err != nil {
		t.Fatal(err)
	}
	if len(pinned.SupportedInterfaces) != 1 || pinned.SupportedInterfaces[0].URL != "https://agents.example.com:443/agents/ns/a" {
		t.Errorf("only the same-origin interface may survive: %+v", pinned.SupportedInterfaces)
	}
	if _, err := a.pinInterfaces(cardWith("https://elsewhere.example.com/a2a")); err == nil {
		t.Error("a card with only foreign interfaces must be refused")
	}
}
