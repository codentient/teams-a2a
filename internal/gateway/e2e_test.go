package gateway_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"

	"github.com/codentient/teams-a2a/internal/bridge"
	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/gateway"
	"github.com/codentient/teams-a2a/internal/store"
	"github.com/codentient/teams-a2a/internal/testagent"
	"github.com/codentient/teams-a2a/internal/testbot"
)

const (
	appID    = "11111111-2222-3333-4444-555555555555"
	tenantID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	alice    = "00000000-0000-0000-0000-00000000a11c"
	bob      = "00000000-0000-0000-0000-000000000b0b"
)

type env struct {
	t       *testing.T
	ms      *testbot.Fake
	agent   *testagent.Agent
	agentTS *httptest.Server
	gw      *httptest.Server
	srv     *gateway.Server
	seq     atomic.Int64
}

type opts struct {
	redis        bool
	mutate       func(*config.Config)
	agentCfg     string
	endorsements []string
	// cardBase is the origin the agent card advertises (default: the test server itself).
	cardBase string
	// bot replaces the route's bot block (default: appId/tenantId inline, secret from env).
	bot string
	// logger captures the gateway's logs (default: discarded).
	logger *slog.Logger
}

func newEnv(t *testing.T, o opts) *env {
	t.Helper()
	e := &env{t: t, ms: testbot.New(), agent: &testagent.Agent{}}
	t.Cleanup(e.ms.Close)
	if o.endorsements != nil {
		e.ms.Endorsements = o.endorsements
	}

	var ts *httptest.Server
	ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := o.cardBase
		if base == "" {
			base = ts.URL
		}
		e.agent.Handler(base, "/agents/kagent/demo").ServeHTTP(w, r)
	}))
	e.agentTS = ts
	t.Cleanup(ts.Close)

	t.Setenv("TEST_BOT_SECRET", "s3cret")
	t.Setenv("TEST_AGENT_TOKEN", "agent-token")
	e.ms.SetClientSecret(appID, "s3cret")
	bot := o.bot
	if bot == "" {
		bot = fmt.Sprintf(`
      appId: %s
      tenantId: %s
      auth: {type: clientSecret, clientSecret: {env: TEST_BOT_SECRET}}`, appID, tenantID)
	}
	agentCfg := strings.ReplaceAll(o.agentCfg, "AGENT_URL", ts.URL)
	if agentCfg == "" {
		agentCfg = fmt.Sprintf(`
      cardURL: %s/agents/kagent/demo/.well-known/agent-card.json
      auth: {type: bearer, token: {env: TEST_AGENT_TOKEN}}
      identityHeaders: {X-User-Id: aadObjectId}`, ts.URL)
	}
	yamlCfg := fmt.Sprintf(`
teams:
  openIdMetadataURL: %[1]s/v1/.well-known/openidconfiguration
  loginAuthority: %[1]s
  serviceUrlAllowlist: ["127.0.0.1"]
  allowInsecureServiceURLs: true
routes:
  - name: demo
    bot: %[5]s
    allowedTenants: [%[3]s]
    allowAllTenantUsers: true
    sessionScope: user
    streamInterval: 1s
    welcome: "Hi! I'm the demo agent."
    agent: %[4]s
`, e.ms.Server.URL, appID, tenantID, agentCfg, bot)
	cfg, err := config.Parse([]byte(yamlCfg))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	if o.mutate != nil {
		o.mutate(cfg)
		if err := cfg.Validate(); err != nil {
			t.Fatalf("config: %v", err)
		}
	}
	var st store.Store = store.NewMemory(time.Hour, 1000)
	if o.redis {
		mr := miniredis.RunT(t)
		st = store.NewRedisFromClient(redis.NewClient(&redis.Options{Addr: mr.Addr()}), "test:", time.Hour)
	}
	t.Cleanup(func() { _ = st.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if o.logger != nil {
		log = o.logger
	}
	if os.Getenv("GATEWAY_TEST_LOGS") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))
	}
	srv, err := gateway.New(cfg, gateway.Deps{Store: st, Logger: log, HTTPClient: http.DefaultClient})
	if err != nil {
		t.Fatalf("gateway: %v", err)
	}
	if err := srv.Warmup(context.Background()); err != nil {
		t.Fatalf("warmup: %v", err)
	}
	srv.SetReady()
	e.srv = srv
	e.gw = httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		e.gw.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return e
}

type activityOpt func(map[string]any)

func groupChat(m map[string]any) {
	m["conversation"] = map[string]any{"id": "19:group@thread.v2", "conversationType": "groupChat", "tenantId": tenantID, "isGroup": true}
}

func from(id, name string) activityOpt {
	return func(m map[string]any) {
		m["from"] = map[string]any{"id": "29:" + id, "aadObjectId": id, "name": name}
	}
}

func value(v map[string]any) activityOpt { return func(m map[string]any) { m["value"] = v } }

func (e *env) activity(text string, opts ...activityOpt) map[string]any {
	id := fmt.Sprintf("in-%d", e.seq.Add(1))
	m := map[string]any{
		"type": "message", "id": id, "channelId": "msteams", "serviceUrl": e.ms.Server.URL + "/",
		"text":         text,
		"from":         map[string]any{"id": "29:" + alice, "aadObjectId": alice, "name": "Alice"},
		"recipient":    map[string]any{"id": "28:" + appID, "name": "demo"},
		"conversation": map[string]any{"id": "a:personal-chat-1", "conversationType": "personal", "tenantId": tenantID},
		"channelData":  map[string]any{"tenant": map[string]any{"id": tenantID}},
		"locale":       "en-US",
	}
	for _, o := range opts {
		o(m)
	}
	return m
}

func (e *env) post(act map[string]any, token string) int {
	e.t.Helper()
	b, _ := json.Marshal(act)
	req, _ := http.NewRequest(http.MethodPost, e.gw.URL+"/api/messages", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatalf("post: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func (e *env) send(act map[string]any) {
	e.t.Helper()
	if code := e.post(act, e.ms.Token(appID, act["serviceUrl"].(string), nil)); code != http.StatusOK {
		e.t.Fatalf("post activity: status %d", code)
	}
}

func (e *env) waitFinal(contains string) testbot.Call {
	e.t.Helper()
	c, ok := e.ms.WaitFor(10*time.Second, func(c testbot.Call) bool {
		return c.Type() == "message" && strings.Contains(c.Text(), contains)
	})
	if !ok {
		e.t.Fatalf("no message containing %q; calls: %s", contains, dump(e.ms.Calls()))
	}
	return c
}

func dump(calls []testbot.Call) string {
	var b strings.Builder
	for _, c := range calls {
		j, _ := json.Marshal(c.Body)
		fmt.Fprintf(&b, "\n  %s %s %s", c.Method, c.Path, j)
	}
	return b.String()
}

func TestStreamingReplyInPersonalChat(t *testing.T) {
	e := newEnv(t, opts{})
	e.agent.ChunkDelay = 400 * time.Millisecond

	e.send(e.activity("hello there"))
	final := e.waitFinal("Echo: hello there")

	calls := e.ms.Calls()
	var streaming []testbot.Call
	for _, c := range calls {
		if c.Type() == "typing" && c.StreamInfo() != nil {
			streaming = append(streaming, c)
		}
	}
	if len(streaming) == 0 {
		t.Fatalf("expected native streaming updates, got: %s", dump(calls))
	}
	if seq := streaming[0].StreamInfo()["streamSequence"]; seq != float64(1) {
		t.Errorf("first streamSequence = %v, want 1", seq)
	}
	si := final.StreamInfo()
	if si == nil || si["streamType"] != "final" || si["streamId"] == "" || si["streamSequence"] != nil {
		t.Errorf("final message must close the stream (streamType final, streamId, no sequence): %v", si)
	}
	for _, c := range calls {
		if c.Auth != "Bearer "+testbot.BotToken {
			t.Errorf("connector call without bot token: %s %s", c.Method, c.Path)
		}
	}
	if !strings.Contains(fmt.Sprint(final.Body["entities"]), "AIGeneratedContent") {
		t.Errorf("final answer should carry the AI-generated label")
	}

	// The agent saw kagent's required headers, the bearer credential and the user identity.
	reqs := e.agent.Requests()
	h := reqs[len(reqs)-1].Header
	if h.Get("A2A-Version") != "1.0" {
		t.Errorf("A2A-Version = %q, want 1.0", h.Get("A2A-Version"))
	}
	if h.Get("Authorization") != "Bearer agent-token" {
		t.Errorf("Authorization = %q", h.Get("Authorization"))
	}
	if h.Get("X-User-Id") != alice {
		t.Errorf("X-User-Id = %q, want %s", h.Get("X-User-Id"), alice)
	}
	if !strings.Contains(h.Get("A2A-Extensions"), testagent.HITLURI) {
		t.Errorf("HITL extension not requested although the card declares it: %q", h.Get("A2A-Extensions"))
	}
}

func TestConversationContextIsReused(t *testing.T) {
	e := newEnv(t, opts{})
	e.send(e.activity("first"))
	e.waitFinal("Echo: first")
	e.send(e.activity("second"))
	e.waitFinal("Echo: second")

	msgs := e.agent.Messages()
	if len(msgs) != 2 {
		t.Fatalf("agent got %d messages, want 2", len(msgs))
	}
	if msgs[0].ContextID != "" {
		t.Errorf("first message should start a new context, got %q", msgs[0].ContextID)
	}
	if msgs[1].ContextID == "" {
		t.Errorf("second message should continue the first context")
	}
	if md := msgs[0].Metadata["teams-a2a-gateway/conversationType"]; md != "personal" {
		t.Errorf("forwarded metadata missing: %v", msgs[0].Metadata)
	}

	e.send(e.activity("/new"))
	e.waitFinal("Started a new conversation")
	e.send(e.activity("third"))
	e.waitFinal("Echo: third")
	if got := e.agent.Messages()[2].ContextID; got != "" {
		t.Errorf("after /new the context should be reset, got %q", got)
	}
}

func cardSubmit(c map[string]any, title string) map[string]any {
	actions, _ := c["actions"].([]any)
	for _, a := range actions {
		m := a.(map[string]any)
		if m["title"] == title {
			return m["data"].(map[string]any)
		}
	}
	return nil
}

func TestToolApprovalFlowInGroupChat(t *testing.T) {
	e := newEnv(t, opts{})
	e.send(e.activity("<at>demo</at> please delete pod web-1", groupChat, func(m map[string]any) {
		m["entities"] = []any{map[string]any{"type": "mention", "text": "<at>demo</at>", "mentioned": map[string]any{"id": "28:" + appID}}}
	}))

	cardCall, ok := e.ms.WaitFor(10*time.Second, func(c testbot.Call) bool { return c.Card() != nil })
	if !ok {
		t.Fatalf("no approval card: %s", dump(e.ms.Calls()))
	}
	if got := e.agent.Messages()[0].Parts[0].Text(); got != "please delete pod web-1" {
		t.Errorf("mention not stripped: %q", got)
	}
	cardJSON, _ := json.Marshal(cardCall.Card())
	for _, want := range []string{"Approval needed", "k8s_delete_resource", "web-1", "Only Alice can answer this."} {
		if !strings.Contains(string(cardJSON), want) {
			t.Errorf("card missing %q: %s", want, cardJSON)
		}
	}
	if !strings.HasSuffix(cardCall.Path, "/activities/in-1") {
		t.Errorf("card should be threaded as a reply to the user message, path %s", cardCall.Path)
	}
	approve := cardSubmit(cardCall.Card(), "Approve")

	// Bob may not answer Alice's request.
	e.ms.Reset()
	e.send(e.activity("", groupChat, from(bob, "Bob"), value(approve)))
	e.waitFinal("Only the person who started this request can answer it.")

	// A forged nonce is rejected.
	forged := map[string]any{bridge.SubmitKey: map[string]any{"nonce": "forged", "action": "approve"}}
	e.send(e.activity("", groupChat, value(forged)))
	e.waitFinal("no longer waiting")

	// Alice approves.
	e.send(e.activity("", groupChat, value(approve)))
	e.waitFinal("Deleted pod web-1.")

	msgs := e.agent.Messages()
	resp := msgs[len(msgs)-1]
	if !slices.Contains(resp.Extensions, testagent.HITLURI) {
		t.Fatalf("approval must declare the HITL extension: %+v", resp)
	}
	payload, _ := json.Marshal(resp.Metadata[testagent.HITLURI])
	if !strings.Contains(string(payload), `"approvals":[{"approved":true,"id":"adk-tool-1"}]`) || !strings.Contains(string(payload), "tool_approval_response") {
		t.Errorf("unexpected approval payload: %s", payload)
	}
	if resp.TaskID == "" || resp.ContextID == "" {
		t.Errorf("approval must target the paused task: task=%q context=%q", resp.TaskID, resp.ContextID)
	}

	// The card was replaced with a summary so it cannot be submitted twice.
	upd, ok := e.ms.WaitFor(5*time.Second, func(c testbot.Call) bool { return c.Method == http.MethodPut })
	if !ok || !strings.Contains(fmt.Sprint(upd.Card()), "Approved") {
		t.Errorf("card not retired: %s", dump(e.ms.Calls()))
	}
	// A replayed submission no longer matches anything.
	e.send(e.activity("", groupChat, value(approve)))
	e.waitFinal("no longer waiting")
}

func TestToolRejectionWithReason(t *testing.T) {
	e := newEnv(t, opts{})
	e.send(e.activity("delete pod web-1"))
	cardCall, ok := e.ms.WaitFor(10*time.Second, func(c testbot.Call) bool { return c.Card() != nil })
	if !ok {
		t.Fatal("no approval card")
	}
	reject := cardSubmit(cardCall.Card(), "Reject")
	reject["reason"] = "not in prod"
	e.send(e.activity("", value(reject)))
	e.waitFinal("Reason: not in prod")
}

func TestAskUserAnsweredByText(t *testing.T) {
	e := newEnv(t, opts{})
	e.send(e.activity("scale the deployment"))
	if _, ok := e.ms.WaitFor(10*time.Second, func(c testbot.Call) bool { return c.Card() != nil }); !ok {
		t.Fatalf("no question card: %s", dump(e.ms.Calls()))
	}
	e.send(e.activity("mars"))
	e.waitFinal("Please pick one of the choices")
	e.send(e.activity("Staging"))
	e.waitFinal(`Scaling in [{"answer":["staging"]}]`)
}

func TestAskUserAnsweredByCard(t *testing.T) {
	e := newEnv(t, opts{})
	e.send(e.activity("scale the deployment"))
	cardCall, ok := e.ms.WaitFor(10*time.Second, func(c testbot.Call) bool { return c.Card() != nil })
	if !ok {
		t.Fatal("no question card")
	}
	answer := cardSubmit(cardCall.Card(), "Send")
	answer["q0"] = "1" // index of "production"
	e.send(e.activity("", value(answer)))
	e.waitFinal(`Scaling in [{"answer":["production"]}]`)
}

func TestAgentFailureIsReported(t *testing.T) {
	e := newEnv(t, opts{})
	e.send(e.activity("please fail"))
	e.waitFinal("couldn't complete this request (failed): model provider returned 503")
}

func TestStreamingFallsBackWhenTeamsRejectsStreaming(t *testing.T) {
	e := newEnv(t, opts{})
	e.agent.ChunkDelay = 300 * time.Millisecond
	e.ms.FailPath("/activities", http.StatusForbidden) // only typing activities fail
	e.send(e.activity("hello"))
	final := e.waitFinal("Echo: hello")
	if final.StreamInfo() != nil {
		t.Errorf("broken stream must be replaced by a plain message")
	}
}

func TestInboundSecurity(t *testing.T) {
	e := newEnv(t, opts{})
	act := e.activity("hi")
	svc := act["serviceUrl"].(string)

	cases := []struct {
		name  string
		token string
		want  int
	}{
		{"no token", "", http.StatusUnauthorized},
		{"wrong audience", e.ms.Token("someone-else", svc, nil), http.StatusUnauthorized},
		{"wrong issuer", e.ms.Token(appID, svc, func(c jwt.MapClaims) { c["iss"] = "https://evil.example" }), http.StatusUnauthorized},
		{"expired", e.ms.Token(appID, svc, func(c jwt.MapClaims) { c["exp"] = time.Now().Add(-time.Hour).Unix() }), http.StatusUnauthorized},
		{"serviceUrl mismatch", e.ms.Token(appID, "https://evil.example/", nil), http.StatusForbidden},
		{"unsigned", "eyJhbGciOiJub25lIiwidHlwIjoiSldUIn0." + jwtPayload(appID, svc) + ".", http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := e.post(act, tc.token); got != tc.want {
				t.Errorf("status = %d, want %d", got, tc.want)
			}
		})
	}

	t.Run("other channel", func(t *testing.T) {
		a := e.activity("hi")
		a["channelId"] = "webchat"
		if got := e.post(a, e.ms.Token(appID, svc, nil)); got != http.StatusForbidden {
			t.Errorf("status = %d, want 403", got)
		}
	})

	time.Sleep(300 * time.Millisecond)
	if n := len(e.agent.Messages()); n != 0 {
		t.Fatalf("agent must not be called for rejected requests, got %d", n)
	}
}

func jwtPayload(aud, svc string) string {
	b, _ := json.Marshal(map[string]any{"iss": testbot.Issuer, "aud": aud, "serviceurl": svc, "exp": time.Now().Add(time.Hour).Unix()})
	return strings.TrimRight(base64URL(b), "=")
}

func TestUnendorsedKeyIsRejected(t *testing.T) {
	e := newEnv(t, opts{endorsements: []string{"webchat"}})
	act := e.activity("hi")
	if got := e.post(act, e.ms.Token(appID, act["serviceUrl"].(string), nil)); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", got)
	}
}

func TestTenantAllowlistAndDedupe(t *testing.T) {
	e := newEnv(t, opts{redis: true})

	foreign := e.activity("hi from elsewhere")
	foreign["channelData"] = map[string]any{"tenant": map[string]any{"id": "another-tenant"}}
	e.send(foreign)

	dup := e.activity("only once")
	e.send(dup)
	e.send(dup) // Bot Service redelivery
	e.waitFinal("Echo: only once")
	time.Sleep(500 * time.Millisecond)

	msgs := e.agent.Messages()
	if len(msgs) != 1 {
		t.Fatalf("agent got %d messages, want exactly 1 (foreign tenant dropped, duplicate ignored)", len(msgs))
	}
}

func TestWelcomeMessage(t *testing.T) {
	e := newEnv(t, opts{})
	act := e.activity("")
	act["type"] = "conversationUpdate"
	act["membersAdded"] = []any{map[string]any{"id": "28:" + appID}}
	e.send(act)
	e.waitFinal("Hi! I'm the demo agent.")
}

func TestCrossOriginCardIsRefused(t *testing.T) {
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "x", "description": "x", "version": "1",
			"supportedInterfaces": []any{map[string]any{"url": "https://attacker.example/a2a", "protocolBinding": "JSONRPC", "protocolVersion": "1.0"}},
			"capabilities":        map[string]any{"streaming": true},
			"defaultInputModes":   []string{"text"}, "defaultOutputModes": []string{"text"},
		})
	}))
	defer evil.Close()
	e := newEnv(t, opts{agentCfg: fmt.Sprintf(`
      cardURL: %s/.well-known/agent-card.json
      auth: {type: bearer, token: {env: TEST_AGENT_TOKEN}}`, evil.URL)})
	e.send(e.activity("hello"))
	e.waitFinal("couldn't get an answer from the agent")
}

func TestInClusterCardNeedsEndpointOverride(t *testing.T) {
	// kagent 1.0 cards advertise the controller's in-cluster address.
	const inCluster = "http://kagent-controller.kagent.svc:8083"

	without := newEnv(t, opts{cardBase: inCluster, agentCfg: `
      cardURL: AGENT_URL/agents/kagent/demo/.well-known/agent-card.json`})
	without.send(without.activity("hello"))
	without.waitFinal("couldn't get an answer from the agent")

	with := newEnv(t, opts{cardBase: inCluster, agentCfg: `
      cardURL: AGENT_URL/agents/kagent/demo/.well-known/agent-card.json
      endpoint: {url: AGENT_URL/agents/kagent/demo}
      hitl: never`})
	with.send(with.activity("override works"))
	with.waitFinal("Echo: override works")
	h := with.agent.Requests()[0].Header
	if h.Get("A2A-Extensions") != "" {
		t.Errorf("hitl: never must not request the extension")
	}
	if h.Get("Authorization") != "" || h.Get("X-User-Id") != "" {
		t.Errorf("no credentials or identity configured, got %v", h)
	}
}

func base64URL(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
