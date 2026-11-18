// Package gateway exposes the Bot Service messaging endpoint and wires each configured
// route (one Teams bot -> one A2A agent) to the bridge.
package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/codentient/teams-a2a/internal/agent"
	"github.com/codentient/teams-a2a/internal/bridge"
	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/identity"
	"github.com/codentient/teams-a2a/internal/metrics"
	"github.com/codentient/teams-a2a/internal/store"
	"github.com/codentient/teams-a2a/internal/teams"
)

const dedupeTTL = 30 * time.Minute

type route struct {
	cfg       config.Route
	agent     *agent.Agent
	bridge    *bridge.Bridge
	connector *teams.Connector
	limiter   *userLimiter
}

// Server handles Bot Service traffic for every configured route.
type Server struct {
	cfg       *config.Config
	log       *slog.Logger
	validator *teams.Validator
	store     store.Store
	metrics   *metrics.Metrics
	routes    map[string]*route // lower-case bot appId
	signers   []*agent.UserJWTSigner

	sem     chan struct{}
	wg      sync.WaitGroup
	baseCtx context.Context
	cancel  context.CancelFunc
	ready   bool
	readyMu sync.RWMutex
}

// Deps lets tests and main inject shared infrastructure.
type Deps struct {
	Store   store.Store
	Metrics *metrics.Metrics
	Logger  *slog.Logger
	// HTTPClient is used for Microsoft endpoints and agents unless overridden.
	HTTPClient *http.Client
	// AgentHTTPClient, if set, is used for A2A calls (long-lived streams need no overall timeout).
	AgentHTTPClient *http.Client
}

func New(cfg *config.Config, d Deps) (*Server, error) {
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Metrics == nil {
		d.Metrics = metrics.New()
	}
	if d.AgentHTTPClient == nil {
		d.AgentHTTPClient = d.HTTPClient
	}
	appIDs := make([]string, 0, len(cfg.Routes))
	for _, r := range cfg.Routes {
		appIDs = append(appIDs, r.Bot.AppID)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:       cfg,
		log:       d.Logger,
		validator: teams.NewValidator(cfg.Teams.OpenIDMetadataURL, cfg.Teams.Issuer, appIDs, cfg.Teams.AllowedChannels, d.HTTPClient),
		store:     d.Store,
		metrics:   d.Metrics,
		routes:    map[string]*route{},
		sem:       make(chan struct{}, cfg.Server.MaxConcurrentTurns),
		baseCtx:   ctx,
		cancel:    cancel,
	}
	policy := teams.ServiceURLPolicy{Allowlist: cfg.Teams.ServiceURLAllowlist, AllowInsecure: cfg.Teams.AllowInsecureServiceURLs}
	for _, rc := range cfg.Routes {
		tokens, err := teams.NewTokenSource(cfg.Teams, rc.Bot, d.HTTPClient)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("route %s: %w", rc.Name, err)
		}
		creds, signer, err := agent.NewCredentials(rc.Agent.Auth, d.HTTPClient)
		if err != nil {
			cancel()
			return nil, fmt.Errorf("route %s: %w", rc.Name, err)
		}
		if signer != nil {
			s.signers = append(s.signers, signer)
		}
		conn := teams.NewConnector(tokens, policy, d.HTTPClient)
		conn.Observe = d.Metrics.ObserveConnector
		ag := agent.New(rc.Agent, creds, d.AgentHTTPClient)
		name := rc.Name
		br := bridge.New(bridge.Options{
			Store:            d.Store,
			Agent:            ag,
			TurnTimeout:      rc.TurnTimeout.Duration,
			ShowToolActivity: rc.ToolActivityEnabled(),
			ApprovalPolicy:   rc.ApprovalPolicy,
			ForwardMetadata:  rc.Agent.ForwardMetadataEnabled(),
			Logger:           d.Logger.With("route", name),
			ObserveTurn: func(outcome string, total, first time.Duration) {
				d.Metrics.Turns.WithLabelValues(name, outcome).Inc()
				d.Metrics.TurnDuration.WithLabelValues(name).Observe(total.Seconds())
				if first > 0 {
					d.Metrics.FirstOutput.WithLabelValues(name).Observe(first.Seconds())
				}
			},
		})
		s.routes[strings.ToLower(rc.Bot.AppID)] = &route{
			cfg: rc, agent: ag, bridge: br, connector: conn,
			limiter: newUserLimiter(rc.RateLimit.PerMinute, rc.RateLimit.Burst),
		}
	}
	return s, nil
}

// Warmup fetches signing keys and agent cards so configuration errors surface at startup.
// Agent card failures are logged, not fatal: the agent may come up after the gateway.
// It does not mark the server ready: call SetReady once the public listener accepts traffic.
func (s *Server) Warmup(ctx context.Context) error {
	if err := s.validator.Refresh(ctx); err != nil {
		return err
	}
	for _, r := range s.routes {
		if card, err := r.agent.Card(ctx); err != nil {
			s.log.Warn("agent card not available yet", "route", r.cfg.Name, "err", err)
		} else {
			s.log.Info("agent ready", "route", r.cfg.Name, "agent", card.Name, "streaming", card.Capabilities.Streaming, "hitl", r.agent.HITL(card))
		}
	}
	return nil
}

// SetReady makes /readyz report ready. Call it only after the public listener is bound, so a
// probe can never pass while /api/messages still refuses connections.
func (s *Server) SetReady() {
	s.readyMu.Lock()
	s.ready = true
	s.readyMu.Unlock()
}

// Handler is the public Bot Service endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/messages", s.handleMessages)
	return mux
}

// AdminHandler serves health, metrics and the user-JWT JWKS. Do not expose it publicly.
func (s *Server) AdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		s.readyMu.RLock()
		ready := s.ready
		s.readyMu.RUnlock()
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if !ready || s.store.Ping(ctx) != nil {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	mux.Handle("GET /metrics", promhttp.HandlerFor(s.metrics.Registry, promhttp.HandlerOpts{}))
	mux.HandleFunc("GET /.well-known/jwks.json", func(w http.ResponseWriter, _ *http.Request) {
		keys := []any{}
		for _, sg := range s.signers {
			keys = append(keys, sg.JWKS()["keys"].([]map[string]any)[0])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "public, max-age=300")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	return mux
}

func (s *Server) handleMessages(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
	var act teams.Activity
	if err := json.NewDecoder(r.Body).Decode(&act); err != nil {
		http.Error(w, "invalid activity", http.StatusBadRequest)
		return
	}
	claims, err := s.validator.Validate(r.Context(), r.Header.Get("Authorization"), &act)
	if err != nil {
		status := http.StatusUnauthorized
		var ae *teams.AuthError
		if errors.As(err, &ae) {
			status = ae.Status
		}
		s.metrics.AuthFailures.WithLabelValues(fmt.Sprint(status)).Inc()
		s.log.Warn("rejected inbound request", "status", status, "reason", err.Error(), "remote", r.RemoteAddr)
		http.Error(w, http.StatusText(status), status)
		return
	}
	rt := s.routes[strings.ToLower(claims.AppID)]
	if act.Recipient != nil && act.Recipient.ID != "" && !strings.HasSuffix(strings.ToLower(act.Recipient.ID), strings.ToLower(claims.AppID)) {
		s.metrics.AuthFailures.WithLabelValues("403").Inc()
		http.Error(w, "recipient does not match token audience", http.StatusForbidden)
		return
	}
	result := s.dispatch(r.Context(), rt, &act)
	s.metrics.Activities.WithLabelValues(rt.cfg.Name, act.Type, result).Inc()
	if act.Type == teams.ActivityInvoke {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":200}`))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// dispatch decides what to do with a validated activity and returns a metrics label.
// It never blocks on the agent: Bot Service expects an answer within 15 seconds.
func (s *Server) dispatch(ctx context.Context, rt *route, act *teams.Activity) string {
	tenant := act.TenantID()
	if !slices.Contains(rt.cfg.AllowedTenants, tenant) {
		s.log.Warn("dropping activity from a tenant that is not allowed", "route", rt.cfg.Name, "tenant", tenant)
		return "tenant_denied"
	}
	if act.Conversation == nil || act.Conversation.ID == "" {
		return "ignored"
	}
	if act.ID != "" {
		first, err := s.store.FirstDelivery(ctx, rt.cfg.Name+":"+act.ID, dedupeTTL)
		if err == nil && !first {
			return "duplicate"
		}
	}
	switch act.Type {
	case teams.ActivityMessage:
		return s.onMessage(rt, act)
	case teams.ActivityConversationUpdate:
		if rt.cfg.Welcome != "" && act.Recipient != nil && slices.ContainsFunc(act.MembersAdded, func(m teams.ChannelAccount) bool {
			return m.ID == act.Recipient.ID
		}) {
			s.background(func(ctx context.Context) {
				_, err := rt.connector.SendToConversation(ctx, act.ServiceURL, act.Conversation.ID, teams.MessageActivity(rt.cfg.Welcome))
				if err != nil {
					s.log.Warn("send welcome", "route", rt.cfg.Name, "err", err)
				}
			}, 30*time.Second)
			return "welcome"
		}
	}
	return "ignored"
}

// sessionKey scopes agent sessions. owner is the user whose session is addressed: the
// sender, or for a card submission the user the card was issued to. The key is always bound
// to the route and the conversation the activity arrived in, so a card can only address
// sessions of its own chat; who may answer is then enforced by the bridge.
func (s *Server) sessionKey(rt *route, act *teams.Activity, owner string) string {
	parts := []string{rt.cfg.Name, act.Conversation.ID}
	if rt.cfg.SessionScope == "user" && !act.IsPersonal() {
		parts = append(parts, owner)
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (s *Server) onMessage(rt *route, act *teams.Activity) string {
	if act.From == nil || act.From.AADObjectID == "" {
		return "no_identity" // bots, anonymous meeting guests, etc.
	}
	notice := func(text string) {
		s.background(func(ctx context.Context) {
			_, _ = rt.connector.ReplyToActivity(ctx, act.ServiceURL, act.Conversation.ID, act.ID, teams.MessageActivity(text))
		}, 30*time.Second)
	}
	if len(rt.cfg.AllowedUsers) > 0 && !slices.Contains(rt.cfg.AllowedUsers, act.From.AADObjectID) {
		notice("Sorry, you don't have access to this agent.")
		return "user_denied"
	}
	if !rt.limiter.Allow(rt.cfg.Name + ":" + act.From.AADObjectID) {
		notice("You're sending messages faster than I can handle. Please wait a moment.")
		return "rate_limited"
	}

	turn := bridge.Turn{
		SessionKey: s.sessionKey(rt, act, act.From.AADObjectID),
		User: identity.User{
			ID:            act.From.AADObjectID,
			ChannelUserID: act.From.ID,
			Name:          act.From.Name,
			TenantID:      act.TenantID(),
		},
		Text: act.TextWithoutBotMention(),
	}
	if len(act.Value) > 0 {
		var v map[string]any
		if json.Unmarshal(act.Value, &v) == nil {
			if meta, ours := v[bridge.SubmitKey].(map[string]any); ours {
				turn.Submission = v
				if owner, _ := meta["owner"].(string); owner != "" {
					turn.SessionKey = s.sessionKey(rt, act, owner)
				}
			}
		}
	}
	if turn.Text == "" && turn.Submission == nil {
		notice("I can only handle text messages for now.")
		return "unsupported"
	}
	turn.Metadata = map[string]any{
		"teams-a2a-gateway/channel":          act.ChannelID,
		"teams-a2a-gateway/conversationType": act.Conversation.ConversationType,
		"teams-a2a-gateway/tenantId":         turn.User.TenantID,
		"teams-a2a-gateway/userName":         act.From.Name,
		"teams-a2a-gateway/locale":           act.Locale,
	}

	select {
	case s.sem <- struct{}{}:
	default:
		notice("I'm handling a lot of requests right now. Please try again in a minute.")
		return "overloaded"
	}
	sink := teams.NewSink(teams.SinkOptions{
		Connector:      rt.connector,
		ServiceURL:     act.ServiceURL,
		ConversationID: act.Conversation.ID,
		ReplyToID:      act.ID,
		Stream:         rt.cfg.StreamingEnabled() && act.IsPersonal(),
		Interval:       rt.cfg.StreamInterval.Duration,
		AILabel:        rt.cfg.AILabelEnabled(),
		RequesterOnly:  rt.cfg.ApprovalPolicy == "requester" && !act.IsPersonal(),
		Cards:          teams.CardPolicy{Enabled: rt.cfg.Agent.Cards.Enabled, AllowedHosts: rt.cfg.Agent.Cards.AllowedHosts},
		Logger:         s.log.With("route", rt.cfg.Name),
	})
	s.background(func(ctx context.Context) {
		defer func() { <-s.sem }()
		s.metrics.InFlight.Inc()
		defer s.metrics.InFlight.Dec()
		if err := rt.bridge.Handle(ctx, turn, sink); err != nil {
			s.log.Error("turn failed", "route", rt.cfg.Name, "err", err)
		}
	}, rt.cfg.TurnTimeout.Duration+2*time.Minute)
	return "accepted"
}

// background runs work detached from the inbound request but tracked for graceful shutdown.
func (s *Server) background(fn func(ctx context.Context), timeout time.Duration) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("panic in background work", "panic", r)
			}
		}()
		ctx, cancel := context.WithTimeout(s.baseCtx, timeout)
		defer cancel()
		fn(ctx)
	}()
}

// Shutdown waits for running turns, then cancels whatever is left.
func (s *Server) Shutdown(ctx context.Context) {
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		s.log.Warn("shutdown timeout reached; cancelling running turns")
		s.cancel()
		<-done
	}
	s.cancel()
}
