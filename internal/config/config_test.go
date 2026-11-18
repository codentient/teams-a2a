package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const minimal = `
routes:
  - name: k8s
    bot:
      appId: app-1
      tenantId: tenant-1
      auth: {type: workloadIdentity}
    allowedTenants: [tenant-1]
    allowAllTenantUsers: true
    agent:
      cardURL: http://kagent-controller.kagent.svc:8083/agents/kagent/k8s-agent/.well-known/agent-card.json
      auth: {type: tokenFile, tokenFile: /var/run/secrets/kagent/token}
`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	r := c.Routes[0]
	if c.Server.Listen != ":3978" || c.Store.Type != "memory" || c.Teams.Issuer != "https://api.botframework.com" {
		t.Errorf("unexpected defaults: %+v %+v", c.Server, c.Teams)
	}
	if !r.StreamingEnabled() || r.SessionScope != "conversation" || r.Agent.HITL != "auto" || r.ApprovalPolicy != "requester" {
		t.Errorf("unexpected route defaults: %+v", r)
	}
	if c.Teams.AllowedChannels[0] != "msteams" {
		t.Errorf("only msteams should be accepted by default")
	}
}

func TestValidationErrors(t *testing.T) {
	cases := map[string]struct {
		replace, with, want string
	}{
		"missing tenant allowlist": {"allowedTenants: [tenant-1]", "", "allowedTenants"},
		"unknown field":            {"allowedTenants:", "allowedTenantz:", "unknown field"},
		"bad scope":                {"allowedTenants: [tenant-1]", "allowedTenants: [tenant-1]\n    sessionScope: team", "sessionScope"},
		"fast streaming":           {"allowedTenants: [tenant-1]", "allowedTenants: [tenant-1]\n    streamInterval: 200ms", "streamInterval"},
		"static auth header": {"auth: {type: tokenFile, tokenFile: /var/run/secrets/kagent/token}",
			"auth: {type: none}\n      headers: {Authorization: Bearer x}", "agent.auth"},
		"identity header overwriting auth": {"auth: {type: tokenFile, tokenFile: /var/run/secrets/kagent/token}",
			"identityHeaders: {Authorization: aadObjectId}", "Authorization"},
		"two secret sources": {"auth: {type: workloadIdentity}",
			"auth: {type: clientSecret, clientSecret: {env: A, file: /b}}", "exactly one"},
		"bad identity field": {"auth: {type: tokenFile, tokenFile: /var/run/secrets/kagent/token}",
			"identityHeaders: {X-User-Id: email}", "X-User-Id"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(strings.Replace(minimal, tc.replace, tc.with, 1)))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDuplicateAppIDs(t *testing.T) {
	two := minimal + strings.Replace(strings.SplitN(minimal, "routes:\n", 2)[1], "name: k8s", "name: other", 1)
	_, err := Parse([]byte(two))
	if err == nil || !strings.Contains(err.Error(), "used by another route") {
		t.Fatalf("got %v", err)
	}
}

func TestSecretRef(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "s")
	if err := os.WriteFile(f, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SECRET_X", "from-env")
	for _, tc := range []struct {
		ref  SecretRef
		want string
	}{
		{SecretRef{Value: "s3cr3t-val"}, "s3cr3t-val"},
		{SecretRef{Env: "SECRET_X"}, "from-env"},
		{SecretRef{File: f}, "from-file"},
	} {
		got, err := tc.ref.Resolve()
		if err != nil || got != tc.want {
			t.Errorf("%v: got %q, %v", tc.ref, got, err)
		}
		if strings.Contains(tc.ref.String(), tc.want) && tc.ref.Value != "" {
			t.Errorf("String() must not reveal the secret")
		}
	}
	if _, err := (&SecretRef{Env: "NOPE_UNSET"}).Resolve(); err == nil {
		t.Error("unset env must fail")
	}
}

func TestExampleConfigsAreValid(t *testing.T) {
	files, _ := filepath.Glob("../../examples/*.yaml")
	if len(files) == 0 {
		t.Fatal("no example configs found")
	}
	for _, f := range files {
		if _, err := Load(f); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}

func TestNoRoutesIsValid(t *testing.T) {
	c, err := Parse([]byte("server: {listen: \":3978\"}\n"))
	if err != nil {
		t.Fatalf("a gateway without routes must be valid (bots are added later): %v", err)
	}
	if len(c.Routes) != 0 {
		t.Fatalf("routes = %d", len(c.Routes))
	}
}

func TestAgentCardsConfig(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Routes[0].Agent.Cards.Enabled {
		t.Fatal("agent cards must be disabled by default")
	}
	agent := "auth: {type: tokenFile, tokenFile: /var/run/secrets/kagent/token}"
	ok := strings.Replace(minimal, agent, agent+"\n      cards: {enabled: true, allowedHosts: [docs.example.com, \"*.payretailers.com\"]}", 1)
	if c, err := Parse([]byte(ok)); err != nil || !c.Routes[0].Agent.Cards.Enabled {
		t.Fatalf("valid cards config rejected: %v", err)
	}
	for _, bad := range []string{"*", "*.com", "https://docs.example.com", "docs.example.com/path", "docs.example.com:443", "localhost"} {
		cfg := strings.Replace(minimal, agent, agent+"\n      cards: {enabled: true, allowedHosts: [\""+bad+"\"]}", 1)
		if _, err := Parse([]byte(cfg)); err == nil || !strings.Contains(err.Error(), "allowedHosts") {
			t.Errorf("allowedHosts %q must be rejected, got %v", bad, err)
		}
	}
}

func TestApprovalTTL(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.Routes[0].ApprovalTTL.Duration != 30*time.Minute {
		t.Fatalf("default approvalTTL = %v, want 30m", c.Routes[0].ApprovalTTL.Duration)
	}
	for _, v := range []string{"30s", "48h"} {
		cfg := strings.Replace(minimal, "allowedTenants: [tenant-1]", "allowedTenants: [tenant-1]\n    approvalTTL: "+v, 1)
		if _, err := Parse([]byte(cfg)); err == nil || !strings.Contains(err.Error(), "approvalTTL") {
			t.Errorf("approvalTTL %s must be rejected, got %v", v, err)
		}
	}
}

// Security review M4: who may use an agent must be an explicit choice.
func TestRouteRequiresAnExplicitAccessChoice(t *testing.T) {
	none := strings.Replace(minimal, "\n    allowAllTenantUsers: true", "", 1)
	if _, err := Parse([]byte(none)); err == nil || !strings.Contains(err.Error(), "allowAllTenantUsers") {
		t.Fatalf("a route without allowedUsers or allowAllTenantUsers must be rejected, got %v", err)
	}
	users := strings.Replace(none, "allowedTenants: [tenant-1]", "allowedTenants: [tenant-1]\n    allowedUsers: [user-1]", 1)
	if _, err := Parse([]byte(users)); err != nil {
		t.Fatalf("allowedUsers alone must be accepted: %v", err)
	}
	if _, err := Parse([]byte(minimal)); err != nil {
		t.Fatalf("allowAllTenantUsers alone must be accepted: %v", err)
	}
	both := strings.Replace(minimal, "allowedTenants: [tenant-1]", "allowedTenants: [tenant-1]\n    allowedUsers: [user-1]", 1)
	if _, err := Parse([]byte(both)); err == nil || !strings.Contains(err.Error(), "allowedUsers") {
		t.Fatalf("allowedUsers together with allowAllTenantUsers is contradictory, got %v", err)
	}
}
