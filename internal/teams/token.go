package teams

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/codentient/teams-a2a/internal/config"
)

// TokenSource returns a bearer token for outbound Bot Connector calls.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

type tokenResponse struct {
	AccessToken string          `json:"access_token"`
	ExpiresIn   json.RawMessage `json:"expires_in"`
	ExpiresOn   json.RawMessage `json:"expires_on"`
	Error       string          `json:"error"`
	ErrorDesc   string          `json:"error_description"`
}

func (t *tokenResponse) lifetime(now time.Time) time.Duration {
	parse := func(raw json.RawMessage) int64 {
		s := strings.Trim(string(raw), `"`)
		var n int64
		_, _ = fmt.Sscan(s, &n)
		return n
	}
	if n := parse(t.ExpiresIn); n > 0 {
		return time.Duration(n) * time.Second
	}
	if n := parse(t.ExpiresOn); n > 0 {
		return time.Unix(n, 0).Sub(now)
	}
	return 5 * time.Minute
}

// cachingSource caches a token until shortly before it expires and collapses concurrent refreshes.
type cachingSource struct {
	fetch func(ctx context.Context) (string, time.Duration, error)

	mu     sync.Mutex
	token  string
	expiry time.Time
}

func (c *cachingSource) Token(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Until(c.expiry) > 5*time.Minute {
		return c.token, nil
	}
	tok, ttl, err := c.fetch(ctx)
	if err != nil {
		if c.token != "" && time.Until(c.expiry) > 30*time.Second {
			return c.token, nil // keep serving the still-valid token through an IdP blip
		}
		return "", err
	}
	c.token, c.expiry = tok, time.Now().Add(ttl)
	return tok, nil
}

func postForm(ctx context.Context, hc *http.Client, endpoint string, form url.Values) (string, time.Duration, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return doTokenRequest(hc, req)
}

func doTokenRequest(hc *http.Client, req *http.Request) (string, time.Duration, error) {
	resp, err := hc.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	var tr tokenResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&tr); err != nil {
		return "", 0, fmt.Errorf("token endpoint returned status %d with an unreadable body", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		// error_description never contains the secret; it helps diagnose misconfiguration.
		return "", 0, fmt.Errorf("token endpoint status %d: %s %s", resp.StatusCode, tr.Error, tr.ErrorDesc)
	}
	return tr.AccessToken, tr.lifetime(time.Now()), nil
}

// NewTokenSource builds the outbound credential for a bot according to its auth type.
func NewTokenSource(teamsCfg config.Teams, bot config.Bot, hc *http.Client) (TokenSource, error) {
	tokenURL := strings.TrimRight(teamsCfg.LoginAuthority, "/") + "/" + url.PathEscape(bot.TenantID) + "/oauth2/v2.0/token"
	scope := teamsCfg.ConnectorScope
	switch bot.Auth.Type {
	case "clientSecret":
		secret := bot.Auth.ClientSecret
		// Fail at startup on a missing env var or unmounted file; the value is re-read per fetch.
		if _, err := secret.Resolve(); err != nil {
			return nil, fmt.Errorf("bot.auth.clientSecret: %w", err)
		}
		return &cachingSource{fetch: func(ctx context.Context) (string, time.Duration, error) {
			s, err := secret.Resolve()
			if err != nil {
				return "", 0, err
			}
			return postForm(ctx, hc, tokenURL, url.Values{
				"grant_type":    {"client_credentials"},
				"client_id":     {bot.AppID},
				"client_secret": {s},
				"scope":         {scope},
			})
		}}, nil
	case "workloadIdentity":
		file := bot.Auth.FederatedTokenFile
		if file == "" {
			file = os.Getenv("AZURE_FEDERATED_TOKEN_FILE")
		}
		if file == "" {
			return nil, errors.New("workloadIdentity needs bot.auth.federatedTokenFile or AZURE_FEDERATED_TOKEN_FILE")
		}
		return &cachingSource{fetch: func(ctx context.Context) (string, time.Duration, error) {
			assertion, err := os.ReadFile(file)
			if err != nil {
				return "", 0, fmt.Errorf("read federated token: %w", err)
			}
			return postForm(ctx, hc, tokenURL, url.Values{
				"grant_type":            {"client_credentials"},
				"client_id":             {bot.AppID},
				"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
				"client_assertion":      {strings.TrimSpace(string(assertion))},
				"scope":                 {scope},
			})
		}}, nil
	case "managedIdentity":
		clientID := bot.Auth.ManagedIdentityClientID
		if clientID == "" {
			clientID = bot.AppID
		}
		resource := strings.TrimSuffix(scope, "/.default")
		return &cachingSource{fetch: func(ctx context.Context) (string, time.Duration, error) {
			ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			q := url.Values{"api-version": {"2018-02-01"}, "resource": {resource}, "client_id": {clientID}}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet,
				"http://169.254.169.254/metadata/identity/oauth2/token?"+q.Encode(), nil)
			if err != nil {
				return "", 0, err
			}
			req.Header.Set("Metadata", "true")
			return doTokenRequest(hc, req)
		}}, nil
	}
	return nil, fmt.Errorf("unsupported bot auth type %q", bot.Auth.Type)
}
