// Package config loads and validates the gateway configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Duration wraps time.Duration so it can be written as "30s" in YAML.
type Duration struct{ time.Duration }

func (d *Duration) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	d.Duration = v
	return nil
}

func (d Duration) MarshalJSON() ([]byte, error) { return []byte(`"` + d.String() + `"`), nil }

type Config struct {
	Server Server  `json:"server"`
	Store  Store   `json:"store"`
	Teams  Teams   `json:"teams"`
	Routes []Route `json:"routes"`
}

type Server struct {
	// Listen is the public address that receives Bot Service traffic.
	Listen string `json:"listen"`
	// AdminListen serves /healthz, /readyz, /metrics and the user-JWT JWKS. Keep it private.
	AdminListen        string   `json:"adminListen"`
	TLSCertFile        string   `json:"tlsCertFile,omitempty"`
	TLSKeyFile         string   `json:"tlsKeyFile,omitempty"`
	MaxBodyBytes       int64    `json:"maxBodyBytes"`
	MaxConcurrentTurns int      `json:"maxConcurrentTurns"`
	ShutdownTimeout    Duration `json:"shutdownTimeout"`
	LogLevel           string   `json:"logLevel"`
}

type Store struct {
	// Type is "memory" (single replica) or "redis" (required for >1 replica).
	Type       string   `json:"type"`
	SessionTTL Duration `json:"sessionTTL"`
	// MaxSessions bounds the memory store.
	MaxSessions int   `json:"maxSessions"`
	Redis       Redis `json:"redis"`
}

type Redis struct {
	Address   string     `json:"address"`
	Username  string     `json:"username,omitempty"`
	Password  *SecretRef `json:"password,omitempty"`
	DB        int        `json:"db"`
	TLS       bool       `json:"tls"`
	KeyPrefix string     `json:"keyPrefix"`
}

type Teams struct {
	// OpenIDMetadataURL is the Bot Connector OpenID document. Override only for tests or sovereign clouds.
	OpenIDMetadataURL string `json:"openIdMetadataURL"`
	// Issuer is the required "iss" of Bot Connector tokens.
	Issuer string `json:"issuer"`
	// LoginAuthority issues outbound tokens, e.g. https://login.microsoftonline.com.
	LoginAuthority string `json:"loginAuthority"`
	// ConnectorScope is the OAuth scope for outbound Connector calls.
	ConnectorScope string `json:"connectorScope"`
	// ServiceURLAllowlist restricts which serviceUrl hosts the gateway will send its bot token to.
	// Entries are exact hosts or "*.suffix" wildcards.
	ServiceURLAllowlist []string `json:"serviceUrlAllowlist"`
	// AllowedChannels lists activity channelIds the gateway accepts.
	AllowedChannels []string `json:"allowedChannels"`
	// AllowInsecureServiceURLs permits http:// serviceUrls. Tests only.
	AllowInsecureServiceURLs bool `json:"allowInsecureServiceURLs,omitempty"`
}

type Route struct {
	Name  string `json:"name"`
	Bot   Bot    `json:"bot"`
	Agent Agent  `json:"agent"`

	// AllowedTenants is required: activities from other Entra tenants are dropped.
	AllowedTenants []string `json:"allowedTenants"`
	// AllowedUsers optionally restricts the route to specific Entra object IDs.
	AllowedUsers []string `json:"allowedUsers,omitempty"`
	// SessionScope is "conversation" (one agent session per Teams chat/thread) or
	// "user" (one session per user per chat/thread; required when the agent scopes
	// sessions to the calling identity, as kagent does).
	SessionScope string `json:"sessionScope"`
	// Streaming enables native Teams streaming in 1:1 chats.
	Streaming *bool `json:"streaming,omitempty"`
	// StreamInterval is the minimum delay between streaming updates (Teams allows 1 req/s).
	StreamInterval Duration `json:"streamInterval"`
	// ShowToolActivity surfaces tool calls as informative updates while the agent works.
	ShowToolActivity *bool `json:"showToolActivity,omitempty"`
	// AILabel marks final answers as AI generated in the Teams UI.
	AILabel *bool `json:"aiLabel,omitempty"`
	// TurnTimeout bounds a single agent turn end to end.
	TurnTimeout Duration `json:"turnTimeout"`
	// Welcome is posted when the bot is added to a conversation. Empty disables it.
	Welcome   string    `json:"welcome,omitempty"`
	RateLimit RateLimit `json:"rateLimit"`
	// ApprovalPolicy decides who may answer a human-in-the-loop prompt:
	// "requester" (default) or "anyone" in the conversation.
	ApprovalPolicy string `json:"approvalPolicy"`
	// ApprovalTTL is how long a tool approval request can be answered. After that any answer
	// rejects it, so a tool never runs on a stale approval.
	ApprovalTTL Duration `json:"approvalTTL"`
}

type RateLimit struct {
	PerMinute float64 `json:"perMinute"`
	Burst     int     `json:"burst"`
}

type Bot struct {
	AppID    string  `json:"appId"`
	TenantID string  `json:"tenantId"`
	Auth     BotAuth `json:"auth"`
}

type BotAuth struct {
	// Type: "clientSecret", "workloadIdentity" (federated token file, e.g. AKS) or "managedIdentity" (IMDS).
	Type         string     `json:"type"`
	ClientSecret *SecretRef `json:"clientSecret,omitempty"`
	// FederatedTokenFile defaults to $AZURE_FEDERATED_TOKEN_FILE.
	FederatedTokenFile string `json:"federatedTokenFile,omitempty"`
	// ManagedIdentityClientID selects a user-assigned identity; defaults to the bot appId.
	ManagedIdentityClientID string `json:"managedIdentityClientId,omitempty"`
}

type Agent struct {
	// CardURL is the agent card location. A URL with no path resolves /.well-known/agent-card.json.
	// kagent 1.0: http://kagent-controller.kagent.svc:8083/agents/<ns>/<name>/.well-known/agent-card.json
	CardURL string `json:"cardURL"`
	// Endpoint overrides the interfaces advertised by the card (e.g. when the card advertises an
	// in-cluster address that the gateway cannot reach).
	Endpoint *Endpoint `json:"endpoint,omitempty"`
	// AllowCrossOriginInterfaces lets the card point the gateway (and its credentials) at hosts
	// other than the card's own host. Off by default to prevent credential exfiltration.
	AllowCrossOriginInterfaces bool `json:"allowCrossOriginInterfaces,omitempty"`
	// CardRefreshInterval controls how often the card is re-fetched.
	CardRefreshInterval Duration  `json:"cardRefreshInterval"`
	Auth                AgentAuth `json:"auth"`
	// IdentityHeaders forwards the Teams user to the agent as headers, e.g. {"X-User-Id": "aadObjectId"}.
	// Values: aadObjectId, tenantId, userName, teamsUserId. Only for trusted networks: the agent
	// cannot verify them.
	IdentityHeaders map[string]string `json:"identityHeaders,omitempty"`
	// Headers are static extra headers.
	Headers map[string]string `json:"headers,omitempty"`
	// HITL controls the kagent human-in-the-loop extension: "auto" (when the card declares it),
	// "always" or "never".
	HITL string `json:"hitl"`
	// ForwardMetadata adds Teams context to the A2A message metadata under "teams-a2a-gateway/...".
	ForwardMetadata *bool `json:"forwardMetadata,omitempty"`
	// Cards controls agent-supplied Adaptive Cards. They are not shown unless enabled.
	Cards Cards `json:"cards"`
}

// Cards controls agent-supplied Adaptive Cards. Agent output is untrusted: cards are dropped
// unless enabled, and enabled cards are reduced to display elements, images from data URLs or
// AllowedHosts, and links to AllowedHosts. Buttons that send data or invoke the bot are removed.
type Cards struct {
	Enabled bool `json:"enabled"`
	// AllowedHosts are hosts (exact or "*.suffix") whose https URLs may appear in Image elements
	// and Action.OpenUrl. Empty: only data: images and no links.
	AllowedHosts []string `json:"allowedHosts,omitempty"`
}

type Endpoint struct {
	URL string `json:"url"`
	// Binding is JSONRPC (default) or HTTP+JSON.
	Binding string `json:"binding"`
	// ProtocolVersion is "1.0" (default) or "0.3".
	ProtocolVersion string `json:"protocolVersion"`
}

type AgentAuth struct {
	// Type: none, bearer, tokenFile, oauth2ClientCredentials, userJWT.
	Type string `json:"type"`
	// Token is a static bearer token (type bearer).
	Token *SecretRef `json:"token,omitempty"`
	// TokenFile is re-read periodically (type tokenFile), e.g. a projected ServiceAccount token
	// with audience "kagent-invocation".
	TokenFile string `json:"tokenFile,omitempty"`
	// OAuth2 configures the client-credentials grant (type oauth2ClientCredentials).
	OAuth2 *OAuth2 `json:"oauth2,omitempty"`
	// UserJWT makes the gateway mint a short-lived, signed JWT per Teams user (type userJWT).
	UserJWT *UserJWT `json:"userJWT,omitempty"`
}

type OAuth2 struct {
	TokenURL     string     `json:"tokenURL"`
	ClientID     string     `json:"clientId"`
	ClientSecret *SecretRef `json:"clientSecret"`
	Scopes       []string   `json:"scopes,omitempty"`
}

type UserJWT struct {
	Issuer   string   `json:"issuer"`
	Audience []string `json:"audience"`
	// SigningKeyFile is a PEM PKCS#8 EC P-256 private key.
	SigningKeyFile string   `json:"signingKeyFile"`
	TTL            Duration `json:"ttl"`
	// Subject selects the identity placed in "sub": aadObjectId (default) or teamsUserId.
	Subject string `json:"subject"`
}

var defaultServiceURLAllowlist = []string{
	"smba.trafficmanager.net",
	"*.botframework.com",
	"smba.infra.gcc.teams.microsoft.com",
	"smba.infra.gov.teams.microsoft.us",
}

// Load reads, defaults and validates a YAML configuration file.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(b)
}

func Parse(b []byte) (*Config, error) {
	var c Config
	if err := yaml.UnmarshalStrict(b, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	c.applyDefaults()
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

func boolPtr(b bool) *bool { return &b }

func (c *Config) applyDefaults() {
	s := &c.Server
	if s.Listen == "" {
		s.Listen = ":3978"
	}
	if s.AdminListen == "" {
		s.AdminListen = ":9090"
	}
	if s.MaxBodyBytes == 0 {
		s.MaxBodyBytes = 1 << 20
	}
	if s.MaxConcurrentTurns == 0 {
		s.MaxConcurrentTurns = 256
	}
	if s.ShutdownTimeout.Duration == 0 {
		s.ShutdownTimeout.Duration = 30 * time.Second
	}
	if s.LogLevel == "" {
		s.LogLevel = "info"
	}
	st := &c.Store
	if st.Type == "" {
		st.Type = "memory"
	}
	if st.SessionTTL.Duration == 0 {
		st.SessionTTL.Duration = 7 * 24 * time.Hour
	}
	if st.MaxSessions == 0 {
		st.MaxSessions = 100_000
	}
	if st.Redis.KeyPrefix == "" {
		st.Redis.KeyPrefix = "teams-a2a:"
	}
	t := &c.Teams
	if t.OpenIDMetadataURL == "" {
		t.OpenIDMetadataURL = "https://login.botframework.com/v1/.well-known/openidconfiguration"
	}
	if t.Issuer == "" {
		t.Issuer = "https://api.botframework.com"
	}
	if t.LoginAuthority == "" {
		t.LoginAuthority = "https://login.microsoftonline.com"
	}
	if t.ConnectorScope == "" {
		t.ConnectorScope = "https://api.botframework.com/.default"
	}
	if len(t.ServiceURLAllowlist) == 0 {
		t.ServiceURLAllowlist = slices.Clone(defaultServiceURLAllowlist)
	}
	if len(t.AllowedChannels) == 0 {
		t.AllowedChannels = []string{"msteams"}
	}
	for i := range c.Routes {
		r := &c.Routes[i]
		if r.SessionScope == "" {
			r.SessionScope = "conversation"
		}
		if r.Streaming == nil {
			r.Streaming = boolPtr(true)
		}
		if r.StreamInterval.Duration == 0 {
			r.StreamInterval.Duration = 1500 * time.Millisecond
		}
		if r.ShowToolActivity == nil {
			r.ShowToolActivity = boolPtr(true)
		}
		if r.AILabel == nil {
			r.AILabel = boolPtr(true)
		}
		if r.TurnTimeout.Duration == 0 {
			r.TurnTimeout.Duration = 10 * time.Minute
		}
		if r.RateLimit.PerMinute == 0 {
			r.RateLimit.PerMinute = 20
		}
		if r.RateLimit.Burst == 0 {
			r.RateLimit.Burst = 5
		}
		if r.ApprovalPolicy == "" {
			r.ApprovalPolicy = "requester"
		}
		if r.ApprovalTTL.Duration == 0 {
			r.ApprovalTTL.Duration = 30 * time.Minute
		}
		if r.Bot.Auth.Type == "" {
			r.Bot.Auth.Type = "clientSecret"
		}
		a := &r.Agent
		if a.Auth.Type == "" {
			a.Auth.Type = "none"
		}
		if a.HITL == "" {
			a.HITL = "auto"
		}
		if a.CardRefreshInterval.Duration == 0 {
			a.CardRefreshInterval.Duration = 5 * time.Minute
		}
		if a.ForwardMetadata == nil {
			a.ForwardMetadata = boolPtr(true)
		}
		if a.Endpoint != nil {
			if a.Endpoint.Binding == "" {
				a.Endpoint.Binding = "JSONRPC"
			}
			if a.Endpoint.ProtocolVersion == "" {
				a.Endpoint.ProtocolVersion = "1.0"
			}
		}
		if a.Auth.UserJWT != nil {
			if a.Auth.UserJWT.TTL.Duration == 0 {
				a.Auth.UserJWT.TTL.Duration = 5 * time.Minute
			}
			if a.Auth.UserJWT.Subject == "" {
				a.Auth.UserJWT.Subject = "aadObjectId"
			}
		}
	}
}

func oneOf(field, v string, allowed ...string) error {
	if slices.Contains(allowed, v) {
		return nil
	}
	return fmt.Errorf("%s: %q is not one of %v", field, v, allowed)
}

func validURL(field, raw string, requireHTTPS bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("%s: %q is not an absolute http(s) URL", field, raw)
	}
	if requireHTTPS && u.Scheme != "https" {
		return fmt.Errorf("%s: %q must use https", field, raw)
	}
	return nil
}

// validHostPattern accepts "host.domain" or "*.domain.tld": no scheme, path, port or bare wildcard.
func validHostPattern(h string) bool {
	h = strings.TrimPrefix(h, "*.")
	if h == "" || strings.ContainsAny(h, "/:@*?# ") || !strings.Contains(h, ".") {
		return false
	}
	return true
}

var identityFields = []string{"aadObjectId", "tenantId", "userName", "teamsUserId"}

// Validate checks the configuration for errors. It is called by Parse.
func (c *Config) Validate() error {
	var errs []error
	add := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	if (c.Server.TLSCertFile == "") != (c.Server.TLSKeyFile == "") {
		add(errors.New("server: tlsCertFile and tlsKeyFile must be set together"))
	}
	add(oneOf("server.logLevel", c.Server.LogLevel, "debug", "info", "warn", "error"))
	add(oneOf("store.type", c.Store.Type, "memory", "redis"))
	if c.Store.Type == "redis" {
		if c.Store.Redis.Address == "" {
			add(errors.New("store.redis.address is required"))
		}
		if c.Store.Redis.Password != nil {
			add(c.Store.Redis.Password.validate("store.redis.password"))
		}
	}
	add(validURL("teams.openIdMetadataURL", c.Teams.OpenIDMetadataURL, false))
	add(validURL("teams.loginAuthority", c.Teams.LoginAuthority, false))
	names := map[string]bool{}
	appIDs := map[string]bool{}
	for i, r := range c.Routes {
		p := fmt.Sprintf("routes[%d]", i)
		if r.Name == "" {
			add(fmt.Errorf("%s.name is required", p))
		} else {
			p = "route " + r.Name
		}
		if names[r.Name] {
			add(fmt.Errorf("%s: duplicate route name", p))
		}
		names[r.Name] = true
		if r.Bot.AppID == "" {
			add(fmt.Errorf("%s: bot.appId is required", p))
		}
		if appIDs[strings.ToLower(r.Bot.AppID)] {
			add(fmt.Errorf("%s: bot.appId %s is used by another route", p, r.Bot.AppID))
		}
		appIDs[strings.ToLower(r.Bot.AppID)] = true
		if r.Bot.TenantID == "" {
			add(fmt.Errorf("%s: bot.tenantId is required (multi-tenant bots are deprecated)", p))
		}
		if len(r.AllowedTenants) == 0 {
			add(fmt.Errorf("%s: allowedTenants must list at least one tenant", p))
		}
		add(oneOf(p+".sessionScope", r.SessionScope, "conversation", "user"))
		add(oneOf(p+".approvalPolicy", r.ApprovalPolicy, "requester", "anyone"))
		if r.ApprovalTTL.Duration < time.Minute || r.ApprovalTTL.Duration > 24*time.Hour {
			add(fmt.Errorf("%s: approvalTTL must be between 1m and 24h", p))
		}
		if r.StreamInterval.Duration < time.Second {
			add(fmt.Errorf("%s: streamInterval must be >= 1s (Teams throttles streaming at 1 request/second)", p))
		}
		switch r.Bot.Auth.Type {
		case "clientSecret":
			add(r.Bot.Auth.ClientSecret.validate(p + ".bot.auth.clientSecret"))
		case "workloadIdentity", "managedIdentity":
		default:
			add(oneOf(p+".bot.auth.type", r.Bot.Auth.Type, "clientSecret", "workloadIdentity", "managedIdentity"))
		}
		a := r.Agent
		if a.CardURL == "" {
			add(fmt.Errorf("%s: agent.cardURL is required", p))
		} else {
			add(validURL(p+".agent.cardURL", a.CardURL, false))
		}
		if a.Endpoint != nil {
			add(validURL(p+".agent.endpoint.url", a.Endpoint.URL, false))
			add(oneOf(p+".agent.endpoint.binding", a.Endpoint.Binding, "JSONRPC", "HTTP+JSON"))
			add(oneOf(p+".agent.endpoint.protocolVersion", a.Endpoint.ProtocolVersion, "1.0", "0.3"))
		}
		add(oneOf(p+".agent.hitl", a.HITL, "auto", "always", "never"))
		for _, h := range a.Cards.AllowedHosts {
			if !validHostPattern(h) {
				add(fmt.Errorf("%s: agent.cards.allowedHosts entry %q must be a host or *.domain.tld", p, h))
			}
		}
		for h, f := range a.IdentityHeaders {
			if strings.EqualFold(h, "Authorization") {
				add(fmt.Errorf("%s: agent.identityHeaders may not set Authorization", p))
			}
			add(oneOf(p+".agent.identityHeaders."+h, f, identityFields...))
		}
		for h := range a.Headers {
			if strings.EqualFold(h, "Authorization") {
				add(fmt.Errorf("%s: use agent.auth instead of a static Authorization header", p))
			}
		}
		switch a.Auth.Type {
		case "none":
		case "bearer":
			add(a.Auth.Token.validate(p + ".agent.auth.token"))
		case "tokenFile":
			if a.Auth.TokenFile == "" {
				add(fmt.Errorf("%s: agent.auth.tokenFile is required", p))
			}
		case "oauth2ClientCredentials":
			o := a.Auth.OAuth2
			if o == nil {
				add(fmt.Errorf("%s: agent.auth.oauth2 is required", p))
				break
			}
			add(validURL(p+".agent.auth.oauth2.tokenURL", o.TokenURL, false))
			if o.ClientID == "" {
				add(fmt.Errorf("%s: agent.auth.oauth2.clientId is required", p))
			}
			add(o.ClientSecret.validate(p + ".agent.auth.oauth2.clientSecret"))
		case "userJWT":
			u := a.Auth.UserJWT
			if u == nil {
				add(fmt.Errorf("%s: agent.auth.userJWT is required", p))
				break
			}
			if u.Issuer == "" || len(u.Audience) == 0 || u.SigningKeyFile == "" {
				add(fmt.Errorf("%s: agent.auth.userJWT needs issuer, audience and signingKeyFile", p))
			}
			if u.TTL.Duration > time.Hour {
				add(fmt.Errorf("%s: agent.auth.userJWT.ttl must be <= 1h", p))
			}
			add(oneOf(p+".agent.auth.userJWT.subject", u.Subject, "aadObjectId", "teamsUserId"))
		default:
			add(oneOf(p+".agent.auth.type", a.Auth.Type, "none", "bearer", "tokenFile", "oauth2ClientCredentials", "userJWT"))
		}
	}
	return errors.Join(errs...)
}

func (r *Route) StreamingEnabled() bool    { return r.Streaming != nil && *r.Streaming }
func (r *Route) ToolActivityEnabled() bool { return r.ShowToolActivity != nil && *r.ShowToolActivity }
func (r *Route) AILabelEnabled() bool      { return r.AILabel != nil && *r.AILabel }
func (a *Agent) ForwardMetadataEnabled() bool {
	return a.ForwardMetadata != nil && *a.ForwardMetadata
}
