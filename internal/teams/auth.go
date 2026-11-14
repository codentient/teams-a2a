package teams

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// AuthError is returned for a request that fails authentication. Status is the HTTP status
// the gateway should answer with (401 for a missing/invalid token, 403 for a valid token that
// is not acceptable for the activity).
type AuthError struct {
	Status int
	Reason string
}

func (e *AuthError) Error() string { return e.Reason }

func unauthorized(reason string) error {
	return &AuthError{Status: http.StatusUnauthorized, Reason: reason}
}
func forbidden(reason string) error { return &AuthError{Status: http.StatusForbidden, Reason: reason} }

// Claims is the validated identity of an inbound Bot Connector request.
type Claims struct {
	AppID      string // the "aud" claim: which of our bots the request is for
	ServiceURL string
}

// Validator verifies Bot Connector -> bot JWTs as required by
// https://learn.microsoft.com/azure/bot-service/rest-api/bot-framework-rest-connector-authentication.
// There is deliberately no way to turn validation off.
type Validator struct {
	metadataURL string
	issuer      string
	audiences   map[string]string // lower-case appId -> appId
	channels    []string
	httpClient  *http.Client
	now         func() time.Time

	mu          sync.RWMutex
	keys        map[string]*signingKey
	fetchedAt   time.Time
	lastAttempt time.Time
	refreshMu   sync.Mutex
}

type signingKey struct {
	pub          *rsa.PublicKey
	endorsements []string
}

const (
	clockSkew          = 5 * time.Minute
	keyRefreshInterval = 12 * time.Hour // Microsoft requires at least every 24h
	minRefetchInterval = 5 * time.Minute
)

func NewValidator(metadataURL, issuer string, appIDs, channels []string, httpClient *http.Client) *Validator {
	aud := make(map[string]string, len(appIDs))
	for _, id := range appIDs {
		aud[strings.ToLower(id)] = id
	}
	return &Validator{
		metadataURL: metadataURL,
		issuer:      issuer,
		audiences:   aud,
		channels:    channels,
		httpClient:  httpClient,
		now:         time.Now,
		keys:        map[string]*signingKey{},
	}
}

// Validate authenticates a request for the given activity.
func (v *Validator) Validate(ctx context.Context, authHeader string, act *Activity) (*Claims, error) {
	scheme, raw, ok := strings.Cut(authHeader, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(raw) == "" {
		return nil, unauthorized("missing bearer token")
	}
	if act == nil || act.ServiceURL == "" || act.ChannelID == "" {
		return nil, unauthorized("activity is missing serviceUrl or channelId")
	}
	if !slices.Contains(v.channels, act.ChannelID) {
		return nil, forbidden("channel " + act.ChannelID + " is not allowed")
	}

	var usedKey *signingKey
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.issuer),
		jwt.WithLeeway(clockSkew),
		jwt.WithExpirationRequired(),
		jwt.WithTimeFunc(v.now),
	)
	token, err := parser.Parse(strings.TrimSpace(raw), func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		if kid == "" {
			return nil, errors.New("token has no kid")
		}
		k, err := v.key(ctx, kid)
		if err != nil {
			return nil, err
		}
		usedKey = k
		return k.pub, nil
	})
	if err != nil {
		return nil, unauthorized("invalid token: " + err.Error())
	}
	mc, ok := token.Claims.(jwt.MapClaims)
	if !ok {
		return nil, unauthorized("invalid claims")
	}

	aud, err := mc.GetAudience()
	if err != nil || len(aud) != 1 {
		return nil, unauthorized("token must have exactly one audience")
	}
	appID, known := v.audiences[strings.ToLower(aud[0])]
	if !known {
		return nil, unauthorized("token audience is not a configured bot")
	}

	svc, _ := mc["serviceurl"].(string)
	if svc == "" {
		svc, _ = mc["serviceUrl"].(string)
	}
	if svc == "" || !strings.EqualFold(svc, act.ServiceURL) {
		return nil, forbidden("serviceUrl claim does not match the activity")
	}
	if usedKey == nil || !slices.Contains(usedKey.endorsements, act.ChannelID) {
		return nil, forbidden("signing key is not endorsed for channel " + act.ChannelID)
	}
	return &Claims{AppID: appID, ServiceURL: act.ServiceURL}, nil
}

func (v *Validator) key(ctx context.Context, kid string) (*signingKey, error) {
	v.mu.RLock()
	k, ok := v.keys[kid]
	stale := v.now().Sub(v.fetchedAt) > keyRefreshInterval
	v.mu.RUnlock()
	if ok && !stale {
		return k, nil
	}
	// Unknown kid or stale cache: refresh, but never hammer the metadata endpoint.
	if err := v.refresh(ctx, !ok); err != nil && !ok {
		return nil, err
	}
	v.mu.RLock()
	defer v.mu.RUnlock()
	if k, ok := v.keys[kid]; ok {
		return k, nil
	}
	return nil, errors.New("unknown signing key")
}

// Refresh forces a key download. It is used at startup to fail fast on misconfiguration.
func (v *Validator) Refresh(ctx context.Context) error { return v.refresh(ctx, false) }

func (v *Validator) refresh(ctx context.Context, unknownKid bool) error {
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()
	now := v.now()
	v.mu.RLock()
	fresh := now.Sub(v.fetchedAt) <= keyRefreshInterval
	recent := now.Sub(v.lastAttempt) < minRefetchInterval
	v.mu.RUnlock()
	if (fresh && !unknownKid) || (unknownKid && recent) {
		return nil
	}
	v.mu.Lock()
	v.lastAttempt = now
	v.mu.Unlock()

	keys, err := v.fetchKeys(ctx)
	if err != nil {
		return fmt.Errorf("refresh bot connector signing keys: %w", err)
	}
	v.mu.Lock()
	v.keys = keys
	v.fetchedAt = now
	v.mu.Unlock()
	return nil
}

func (v *Validator) getJSON(ctx context.Context, url string, out any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := v.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

type jwk struct {
	Kty          string   `json:"kty"`
	Kid          string   `json:"kid"`
	N            string   `json:"n"`
	E            string   `json:"e"`
	X5c          []string `json:"x5c"`
	Endorsements []string `json:"endorsements"`
}

func (v *Validator) fetchKeys(ctx context.Context) (map[string]*signingKey, error) {
	var meta struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, v.metadataURL, &meta); err != nil {
		return nil, err
	}
	if meta.JWKSURI == "" {
		return nil, errors.New("openid metadata has no jwks_uri")
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := v.getJSON(ctx, meta.JWKSURI, &set); err != nil {
		return nil, err
	}
	out := make(map[string]*signingKey, len(set.Keys))
	for _, k := range set.Keys {
		if k.Kid == "" || (k.Kty != "" && k.Kty != "RSA") {
			continue
		}
		pub, err := rsaKey(k)
		if err != nil {
			continue
		}
		out[k.Kid] = &signingKey{pub: pub, endorsements: k.Endorsements}
	}
	if len(out) == 0 {
		return nil, errors.New("jwks contains no usable RSA keys")
	}
	return out, nil
}

func rsaKey(k jwk) (*rsa.PublicKey, error) {
	if k.N != "" && k.E != "" {
		n, err := base64.RawURLEncoding.DecodeString(k.N)
		if err != nil {
			return nil, err
		}
		e, err := base64.RawURLEncoding.DecodeString(k.E)
		if err != nil {
			return nil, err
		}
		pub := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}
		if pub.N.BitLen() < 2048 {
			return nil, errors.New("rsa key too small")
		}
		return pub, nil
	}
	if len(k.X5c) > 0 {
		der, err := base64.StdEncoding.DecodeString(k.X5c[0])
		if err != nil {
			return nil, err
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, err
		}
		pub, ok := cert.PublicKey.(*rsa.PublicKey)
		if !ok {
			return nil, errors.New("certificate key is not RSA")
		}
		return pub, nil
	}
	return nil, errors.New("key has neither n/e nor x5c")
}
