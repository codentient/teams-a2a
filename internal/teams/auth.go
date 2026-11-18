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
	mrand "math/rand/v2"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/codentient/teams-a2a/internal/httpx"
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

	onRefreshError func(error)
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
	httpClient = httpx.NoRedirects(httpClient)
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
	v.mu.RUnlock()
	// Known keys are used as cached; keeping them fresh is the background loop's job.
	if ok {
		return k, nil
	}
	// Unknown kid (Microsoft rotated its keys): refresh at most once per cooldown, so
	// unauthenticated requests can never drive outbound fetches (security review M5).
	if err := v.refreshAfterCooldown(ctx); err != nil {
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
// Refresh fetches the signing keys now. It is used at startup and by the background loop.
func (v *Validator) Refresh(ctx context.Context) error {
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()
	return v.fetchAndStore(ctx)
}

// refreshAfterCooldown fetches the keys unless a fetch was attempted within minRefetchInterval.
func (v *Validator) refreshAfterCooldown(ctx context.Context) error {
	v.refreshMu.Lock()
	defer v.refreshMu.Unlock()
	v.mu.RLock()
	recent := v.now().Sub(v.lastAttempt) < minRefetchInterval
	v.mu.RUnlock()
	if recent {
		return nil
	}
	return v.fetchAndStore(ctx)
}

func (v *Validator) fetchAndStore(ctx context.Context) error {
	now := v.now()
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

// StartBackgroundRefresh refreshes the signing keys every keyRefreshInterval (with jitter)
// until ctx ends, retrying failures with backoff. onError, if set, receives failures. Microsoft
// requires a refresh at least every 24 hours; requests never trigger one for known keys.
func (v *Validator) StartBackgroundRefresh(ctx context.Context, onError func(error)) {
	v.onRefreshError = onError
	go v.refreshLoop(ctx, keyRefreshInterval, time.Minute)
}

func (v *Validator) refreshLoop(ctx context.Context, every, retry time.Duration) {
	wait := jitter(every)
	backoff := retry
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		if err := v.Refresh(ctx); err != nil {
			if v.onRefreshError != nil {
				v.onRefreshError(err)
			}
			wait = backoff
			backoff = min(backoff*2, every)
			continue
		}
		wait, backoff = jitter(every), retry
	}
}

// jitter returns d minus up to 10%, so replicas do not refresh in lockstep.
func jitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	return d - time.Duration(mrand.Int64N(int64(d)/10+1))
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

// checkRSA applies the same rules to every signing key, whatever its JWK form (security review
// L3): at least 2048 bits and an odd public exponent of at least 3.
func checkRSA(pub *rsa.PublicKey) (*rsa.PublicKey, error) {
	if pub.N.BitLen() < 2048 {
		return nil, errors.New("rsa key too small")
	}
	if pub.E < 3 || pub.E%2 == 0 {
		return nil, errors.New("rsa public exponent must be odd and at least 3")
	}
	return pub, nil
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
		eb := new(big.Int).SetBytes(e)
		if !eb.IsInt64() || eb.Int64() > 1<<31-1 {
			return nil, errors.New("rsa exponent out of range")
		}
		return checkRSA(&rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(eb.Int64())})
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
		return checkRSA(pub)
	}
	return nil, errors.New("key has neither n/e nor x5c")
}
