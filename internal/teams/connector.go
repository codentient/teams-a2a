package teams

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/codentient/teams-a2a/internal/httpx"
)

// ServiceURLPolicy decides which Connector endpoints may receive the bot's bearer token.
// The serviceUrl already comes from a signed claim; this is defence in depth against a
// compromised or misconfigured channel steering the bot token elsewhere.
type ServiceURLPolicy struct {
	Allowlist     []string
	AllowInsecure bool
}

func (p ServiceURLPolicy) Check(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("invalid serviceUrl %q", raw)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("serviceUrl %q must not carry credentials, query or fragment", raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !p.AllowInsecure {
			return nil, fmt.Errorf("serviceUrl %q must use https", raw)
		}
	default:
		return nil, fmt.Errorf("serviceUrl %q has unsupported scheme", raw)
	}
	host := strings.ToLower(u.Hostname())
	if hostAllowed(host, p.Allowlist) {
		return u, nil
	}
	return nil, fmt.Errorf("serviceUrl host %q is not in the allowlist", host)
}

// hostAllowed reports whether host matches an allowlist entry: an exact host or "*.suffix".
func hostAllowed(host string, allowlist []string) bool {
	host = strings.ToLower(host)
	for _, entry := range allowlist {
		entry = strings.ToLower(entry)
		if suffix, ok := strings.CutPrefix(entry, "*."); ok {
			if strings.HasSuffix(host, "."+suffix) {
				return true
			}
		} else if host == entry {
			return true
		}
	}
	return false
}

// ConnectorError is a non-2xx response from the Bot Connector.
type ConnectorError struct {
	Status int
	Code   string
	Body   string
}

func (e *ConnectorError) Error() string {
	return fmt.Sprintf("bot connector status %d %s", e.Status, e.Code)
}

// Connector is a minimal Bot Connector REST client for one bot identity.
type Connector struct {
	tokens TokenSource
	policy ServiceURLPolicy
	hc     *http.Client
	// Observe, if set, is called after every request with the operation and status code.
	Observe func(op string, status int, d time.Duration)
}

func NewConnector(tokens TokenSource, policy ServiceURLPolicy, hc *http.Client) *Connector {
	return &Connector{tokens: tokens, policy: policy, hc: httpx.NoRedirects(hc)}
}

type resourceResponse struct {
	ID string `json:"id"`
}

// SendToConversation posts an activity and returns the id Teams assigned to it.
func (c *Connector) SendToConversation(ctx context.Context, serviceURL, conversationID string, act *Activity) (string, error) {
	return c.do(ctx, "send", http.MethodPost, serviceURL,
		"v3/conversations/"+url.PathEscape(conversationID)+"/activities", act)
}

// ReplyToActivity posts an activity as a reply (threaded in channels).
func (c *Connector) ReplyToActivity(ctx context.Context, serviceURL, conversationID, replyToID string, act *Activity) (string, error) {
	if replyToID == "" {
		return c.SendToConversation(ctx, serviceURL, conversationID, act)
	}
	act.ReplyToID = replyToID
	return c.do(ctx, "reply", http.MethodPost, serviceURL,
		"v3/conversations/"+url.PathEscape(conversationID)+"/activities/"+url.PathEscape(replyToID), act)
}

// UpdateActivity replaces a previously sent activity (used to retire answered cards).
func (c *Connector) UpdateActivity(ctx context.Context, serviceURL, conversationID, activityID string, act *Activity) error {
	act.ID = activityID
	_, err := c.do(ctx, "update", http.MethodPut, serviceURL,
		"v3/conversations/"+url.PathEscape(conversationID)+"/activities/"+url.PathEscape(activityID), act)
	return err
}

func (c *Connector) do(ctx context.Context, op, method, serviceURL, path string, act *Activity) (string, error) {
	base, err := c.policy.Check(serviceURL)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(act)
	if err != nil {
		return "", err
	}
	endpoint := strings.TrimRight(base.String(), "/") + "/" + path

	const maxAttempts = 3
	for attempt := 1; ; attempt++ {
		id, retryAfter, err := c.once(ctx, op, method, endpoint, body)
		if err == nil {
			return id, nil
		}
		var ce *ConnectorError
		retriable := errors.As(err, &ce) && (ce.Status == http.StatusTooManyRequests || ce.Status >= 500)
		if !retriable || attempt == maxAttempts {
			return "", err
		}
		if retryAfter <= 0 {
			retryAfter = time.Duration(attempt) * 500 * time.Millisecond
		}
		if retryAfter > 10*time.Second {
			retryAfter = 10 * time.Second
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(retryAfter):
		}
	}
}

func (c *Connector) once(ctx context.Context, op, method, endpoint string, body []byte) (string, time.Duration, error) {
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return "", 0, fmt.Errorf("acquire bot token: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		if c.Observe != nil {
			c.Observe(op, 0, time.Since(start))
		}
		return "", 0, err
	}
	defer resp.Body.Close()
	if c.Observe != nil {
		c.Observe(op, resp.StatusCode, time.Since(start))
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		var ra time.Duration
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			ra = time.Duration(s) * time.Second
		}
		return "", ra, &ConnectorError{Status: resp.StatusCode, Code: e.Error.Code, Body: string(data)}
	}
	var rr resourceResponse
	_ = json.Unmarshal(data, &rr)
	return rr.ID, 0, nil
}

// Member is a conversation member as returned by the Bot Connector roster API.
type Member struct {
	ID          string `json:"id"`
	AADObjectID string `json:"aadObjectId"`
	// UserRole is "user", "guest" or "anonymous" in Teams.
	UserRole string `json:"userRole"`
}

// GetMember looks up one member of a conversation, for example to learn whether the user is a
// guest. It is not retried: callers that gate access on it treat any error as a refusal.
func (c *Connector) GetMember(ctx context.Context, serviceURL, conversationID, memberID string) (*Member, error) {
	base, err := c.policy.Check(serviceURL)
	if err != nil {
		return nil, err
	}
	endpoint := strings.TrimRight(base.String(), "/") + "/v3/conversations/" + url.PathEscape(conversationID) + "/members/" + url.PathEscape(memberID)
	token, err := c.tokens.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire bot token: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	start := time.Now()
	resp, err := c.hc.Do(req)
	if err != nil {
		if c.Observe != nil {
			c.Observe("get_member", 0, time.Since(start))
		}
		return nil, err
	}
	defer resp.Body.Close()
	if c.Observe != nil {
		c.Observe("get_member", resp.StatusCode, time.Since(start))
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		return nil, &ConnectorError{Status: resp.StatusCode, Body: string(data)}
	}
	var m Member
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("decode member: %w", err)
	}
	return &m, nil
}
