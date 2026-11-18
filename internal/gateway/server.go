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
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"golang.org/x/time/rate"

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
	roles     *roleCache
}

// Server handles Bot Service traffic for every configured route.
type Server struct {
	cfg         *config.Config
	refreshOnce sync.Once
	rejectLog   *rate.Limiter
	userMu      sync.Mutex
	userTurns   map[string]int
	log         *slog.Logger
	validator   *teams.Validator
	store       store.Store
	metrics     *metrics.Metrics
	routes      map[string]*route // lower-case bot appId
	signers     []*agent.UserJWTSigner

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
	// Version is reported as teams_a2a_build_info{version}.
	Version string
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
	d.Metrics.BuildInfo.WithLabelValues(d.Version).Set(1)
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
		rejectLog: rate.NewLimiter(1, 5),
		userTurns: map[string]int{},
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
		ag := agent.New(rc.Agent, creds, agentClient(d.AgentHTTPClient, rc.Agent.ResponseHeaderTimeout.Duration))
		name := rc.Name
		br := bridge.New(bridge.Options{
			Store:            d.Store,
			Agent:            ag,
			TurnTimeout:      rc.TurnTimeout.Duration,
			ShowToolActivity: rc.ToolActivityEnabled(),
			ApprovalPolicy:   rc.ApprovalPolicy,
			ApprovalTTL:      rc.ApprovalTTL.Duration,
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
			roles:   newRoleCache(time.Hour, 10_000),
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
	s.refreshOnce.Do(func() {
		s.validator.StartBackgroundRefresh(s.baseCtx, func(err error) {
			s.log.Warn("background signing-key refresh failed; keeping the current keys", "err", err)
		})
	})
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

// SetNotReady makes /readyz fail again. Call it first on shutdown, so load balancers remove the
// pod before the listener closes and no request lands on a closed port.
func (s *Server) SetNotReady() {
	s.readyMu.Lock()
	s.ready = false
	s.readyMu.Unlock()
}

// Handler is the public Bot Service endpoint.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/messages", s.handleMessages)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
		mux.ServeHTTP(sw, r)
		if r.URL.Path == "/api/messages" {
			code := strconv.Itoa(sw.code)
			s.metrics.HTTPRequests.WithLabelValues(code).Inc()
			s.metrics.HTTPDuration.WithLabelValues(code).Observe(time.Since(start).Seconds())
		}
	})
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
	reject := func(status int, reason string) {
		s.metrics.AuthFailures.WithLabelValues(fmt.Sprint(status)).Inc()
		if s.rejectLog.Allow() { // sampled: unauthenticated traffic must not flood the logs
			s.log.Warn("rejected inbound request", "status", status, "reason", reason, "remote", r.RemoteAddr)
		}
		http.Error(w, http.StatusText(status), status)
	}
	// Refuse requests without a bearer token before reading the body (security review L8).
	if scheme, tok, ok := strings.Cut(r.Header.Get("Authorization"), " "); !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(tok) == "" {
		reject(http.StatusUnauthorized, "missing bearer token")
		return
	}
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
		reject(status, err.Error())
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

	user := act.From.AADObjectID
	if !s.acquireUserTurn(user) {
		notice(fmt.Sprintf("You already have %d requests running. Please wait for one to finish.", s.cfg.Server.MaxTurnsPerUser))
		return "user_busy"
	}
	select {
	case s.sem <- struct{}{}:
	default:
		s.releaseUserTurn(user)
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
		defer func() { <-s.sem; s.releaseUserTurn(user) }()
		s.metrics.InFlight.Inc()
		defer s.metrics.InFlight.Dec()
		if rt.cfg.DenyGuests && !s.memberAllowed(ctx, rt, act) {
			return
		}
		if err := rt.bridge.Handle(ctx, turn, sink); err != nil {
			s.log.Error("turn failed", "route", rt.cfg.Name, "err", err)
		}
	}, rt.cfg.TurnTimeout.Duration+2*time.Minute)
	return "accepted"
}

// acquireUserTurn reserves one of the user's MaxTurnsPerUser concurrent turns (security review
// L1), so a single user cannot hold most of the global MaxConcurrentTurns.
func (s *Server) acquireUserTurn(user string) bool {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if s.userTurns[user] >= s.cfg.Server.MaxTurnsPerUser {
		return false
	}
	s.userTurns[user]++
	return true
}

func (s *Server) releaseUserTurn(user string) {
	s.userMu.Lock()
	defer s.userMu.Unlock()
	if s.userTurns[user]--; s.userTurns[user] <= 0 {
		delete(s.userTurns, user)
	}
}

// statusWriter records the response status for metrics.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) {
	w.code = code
	w.ResponseWriter.WriteHeader(code)
}

// agentClient returns hc, or a copy whose transport waits up to timeout for the agent's first
// response byte, for agents that answer synchronously.
func agentClient(hc *http.Client, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		return hc
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	base, ok := hc.Transport.(*http.Transport)
	if hc.Transport == nil {
		base, ok = http.DefaultTransport.(*http.Transport)
	}
	if !ok {
		return hc
	}
	t := base.Clone()
	t.ResponseHeaderTimeout = timeout
	c := *hc
	c.Transport = t
	return &c
}

// memberAllowed enforces denyGuests: guests are refused, and so is everyone whose role cannot
// be verified. It runs in the background turn because the roster lookup is a network call.
func (s *Server) memberAllowed(ctx context.Context, rt *route, act *teams.Activity) bool {
	reply := func(text string) {
		_, _ = rt.connector.ReplyToActivity(ctx, act.ServiceURL, act.Conversation.ID, act.ID, teams.MessageActivity(text))
	}
	role, ok := rt.roles.get(act.From.AADObjectID)
	if !ok {
		m, err := rt.connector.GetMember(ctx, act.ServiceURL, act.Conversation.ID, act.From.ID)
		if err != nil {
			s.log.Warn("member role lookup failed; refusing", "route", rt.cfg.Name, "err", err)
			reply("I couldn't verify your access right now. Please try again later.")
			return false
		}
		role = strings.ToLower(m.UserRole)
		rt.roles.put(act.From.AADObjectID, role)
	}
	if role == "guest" {
		s.log.Info("guest refused", "route", rt.cfg.Name)
		reply("Sorry, guests can't use this agent.")
		return false
	}
	return true
}

// roleCache remembers Teams member roles for a while, so a guest check is not a roster call
// per message. It is bounded: when full it starts over.
type roleCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	data map[string]roleEntry
}

type roleEntry struct {
	role    string
	expires time.Time
}

func newRoleCache(ttl time.Duration, max int) *roleCache {
	return &roleCache{ttl: ttl, max: max, data: map[string]roleEntry{}}
}

func (c *roleCache) get(user string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[user]
	if !ok || time.Now().After(e.expires) {
		return "", false
	}
	return e.role, true
}

func (c *roleCache) put(user, role string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.data) >= c.max {
		c.data = map[string]roleEntry{}
	}
	c.data[user] = roleEntry{role: role, expires: time.Now().Add(c.ttl)}
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
