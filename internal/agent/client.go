// Package agent wraps the A2A client for one configured agent: card discovery and
// refresh, origin pinning, credentials, identity propagation and extension negotiation.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/a2aproject/a2a-go/v2/a2aclient/agentcard"
	"github.com/a2aproject/a2a-go/v2/a2acompat/a2av0"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/httpx"
	"github.com/codentient/teams-a2a/internal/identity"
)

// HITLExtensionURI is kagent's human-in-the-loop A2A extension.
const HITLExtensionURI = "https://kagent.dev/extensions/hitl/v1"

// Agent is a long-lived handle on one remote A2A agent.
type Agent struct {
	cfg   config.Agent
	creds Credentials
	hc    *http.Client

	mu        sync.Mutex
	card      *a2a.AgentCard
	client    *a2aclient.Client
	fetchedAt time.Time
}

func New(cfg config.Agent, creds Credentials, hc *http.Client) *Agent {
	return &Agent{cfg: cfg, creds: creds, hc: httpx.NoRedirects(hc)}
}

// cardParser accepts both A2A 1.0 cards and legacy 0.3 cards.
func cardParser(b []byte) (*a2a.AgentCard, error) {
	var probe struct {
		SupportedInterfaces json.RawMessage `json:"supportedInterfaces"`
		URL                 string          `json:"url"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return nil, err
	}
	if len(probe.SupportedInterfaces) == 0 && probe.URL != "" {
		return a2av0.NewAgentCardParser()(b)
	}
	return agentcard.DefaultCardParser(b)
}

func sameOrigin(a, b *url.URL) bool {
	port := func(u *url.URL) string {
		if p := u.Port(); p != "" {
			return p
		}
		if u.Scheme == "https" {
			return "443"
		}
		return "80"
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Hostname(), b.Hostname()) && port(a) == port(b)
}

// pinInterfaces applies the endpoint override or drops interfaces on a different origin
// than the card, so a tampered or misconfigured card cannot redirect our credentials.
func (a *Agent) pinInterfaces(card *a2a.AgentCard) (*a2a.AgentCard, error) {
	c := *card
	if ep := a.cfg.Endpoint; ep != nil {
		c.SupportedInterfaces = []*a2a.AgentInterface{{
			URL:             ep.URL,
			ProtocolBinding: a2a.TransportProtocol(ep.Binding),
			ProtocolVersion: a2a.ProtocolVersion(ep.ProtocolVersion),
		}}
		return &c, nil
	}
	if a.cfg.AllowCrossOriginInterfaces {
		return &c, nil
	}
	cardURL, err := url.Parse(a.cfg.CardURL)
	if err != nil {
		return nil, err
	}
	var kept []*a2a.AgentInterface
	for _, iface := range card.SupportedInterfaces {
		u, err := url.Parse(iface.URL)
		if err == nil && sameOrigin(u, cardURL) {
			kept = append(kept, iface)
		}
	}
	if len(kept) == 0 {
		return nil, fmt.Errorf("agent card at %s advertises no interface on the same origin; set agent.endpoint or allowCrossOriginInterfaces", a.cfg.CardURL)
	}
	c.SupportedInterfaces = kept
	return &c, nil
}

func (a *Agent) get(ctx context.Context) (*a2aclient.Client, *a2a.AgentCard, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil && time.Since(a.fetchedAt) < a.cfg.CardRefreshInterval.Duration {
		return a.client, a.card, nil
	}
	card, err := a.fetchCard(ctx)
	if err != nil {
		if a.client != nil {
			// Keep serving with the last good card; the agent itself may still be up.
			a.fetchedAt = time.Now().Add(-a.cfg.CardRefreshInterval.Duration + 30*time.Second)
			return a.client, a.card, nil
		}
		return nil, nil, err
	}
	client, err := a2aclient.NewFromCard(ctx, card,
		a2aclient.WithJSONRPCTransport(a.hc),
		a2aclient.WithRESTTransport(a.hc),
		a2av0.WithJSONRPCTransport(a2av0.JSONRPCTransportConfig{Client: a.hc}),
		a2aclient.WithCallInterceptors(&interceptor{agent: a, card: card}),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create a2a client: %w", err)
	}
	if a.client != nil {
		_ = a.client.Destroy()
	}
	a.client, a.card, a.fetchedAt = client, card, time.Now()
	return client, card, nil
}

func (a *Agent) fetchCard(ctx context.Context) (*a2a.AgentCard, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	resolver := &agentcard.Resolver{Client: a.hc, CardParser: cardParser}
	var opts []agentcard.ResolveOption
	for k, v := range a.cfg.Headers {
		opts = append(opts, agentcard.WithRequestHeader(k, v))
	}
	// Service credentials (not per-user ones) may be needed to read the card.
	if a.cfg.Auth.Type == "bearer" || a.cfg.Auth.Type == "tokenFile" || a.cfg.Auth.Type == "oauth2ClientCredentials" {
		authz, err := a.creds.Authorization(ctx, identity.User{})
		if err != nil {
			return nil, err
		}
		opts = append(opts, agentcard.WithRequestHeader("Authorization", authz))
	}
	card, err := resolver.Resolve(ctx, a.cfg.CardURL, opts...)
	if err != nil {
		return nil, fmt.Errorf("resolve agent card: %w", err)
	}
	return a.pinInterfaces(card)
}

// Card returns the current agent card, fetching it if needed.
func (a *Agent) Card(ctx context.Context) (*a2a.AgentCard, error) {
	_, card, err := a.get(ctx)
	return card, err
}

// HITL reports whether turns should request kagent's human-in-the-loop extension.
func (a *Agent) HITL(card *a2a.AgentCard) bool {
	switch a.cfg.HITL {
	case "always":
		return true
	case "never":
		return false
	}
	if card == nil {
		return false
	}
	return slices.ContainsFunc(card.Capabilities.Extensions, func(e a2a.AgentExtension) bool {
		return e.URI == HITLExtensionURI
	})
}

// Stream sends a message and yields the agent's events. It falls back to a blocking
// SendMessage when the agent does not support streaming.
func (a *Agent) Stream(ctx context.Context, msg *a2a.Message) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		client, card, err := a.get(ctx)
		if err != nil {
			yield(nil, err)
			return
		}
		if a.HITL(card) {
			ctx = withHITL(ctx)
		}
		for ev, err := range client.SendStreamingMessage(ctx, &a2a.SendMessageRequest{Message: msg}) {
			if !yield(ev, err) || err != nil {
				return
			}
		}
	}
}

func (a *Agent) GetTask(ctx context.Context, id string) (*a2a.Task, error) {
	client, card, err := a.get(ctx)
	if err != nil {
		return nil, err
	}
	if a.HITL(card) {
		ctx = withHITL(ctx)
	}
	return client.GetTask(ctx, &a2a.GetTaskRequest{ID: a2a.TaskID(id)})
}

func (a *Agent) Cancel(ctx context.Context, id string) error {
	client, _, err := a.get(ctx)
	if err != nil {
		return err
	}
	_, err = client.CancelTask(ctx, &a2a.CancelTaskRequest{ID: a2a.TaskID(id)})
	return err
}

type hitlKey struct{}

func withHITL(ctx context.Context) context.Context { return context.WithValue(ctx, hitlKey{}, true) }

// interceptor attaches credentials, identity headers and extension activation to every call.
type interceptor struct {
	agent *Agent
	card  *a2a.AgentCard
}

func (i *interceptor) Before(ctx context.Context, req *a2aclient.Request) (context.Context, any, error) {
	if req.ServiceParams == nil {
		req.ServiceParams = a2aclient.ServiceParams{}
	}
	user, _ := identity.From(ctx)
	authz, err := i.agent.creds.Authorization(ctx, user)
	if err != nil {
		return ctx, nil, fmt.Errorf("agent credentials: %w", err)
	}
	set := func(k, v string) { req.ServiceParams[strings.ToLower(k)] = []string{v} }
	for h, v := range i.agent.cfg.Headers {
		set(h, v)
	}
	for h, field := range i.agent.cfg.IdentityHeaders {
		// Always overwrite or clear, so a static header can never impersonate a user.
		if v := user.Field(field); v != "" {
			set(h, v)
		} else {
			delete(req.ServiceParams, strings.ToLower(h))
		}
	}
	if authz != "" {
		set("Authorization", authz)
	}
	if on, _ := ctx.Value(hitlKey{}).(bool); on {
		req.ServiceParams.Append(a2a.SvcParamExtensions, HITLExtensionURI)
	}
	return ctx, nil, nil
}

func (i *interceptor) After(context.Context, *a2aclient.Response) error { return nil }

// ErrNotConfigured is returned when an agent handle is missing.
var ErrNotConfigured = errors.New("agent not configured")
