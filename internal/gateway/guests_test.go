package gateway_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/testbot"
)

// Security review M4: a route can refuse B2B guests, whose activities carry the resource
// tenant id and so pass allowedTenants.
func denyGuests(c *config.Config) { c.Routes[0].DenyGuests = true }

func TestGuestsAreRefusedWhenDenyGuests(t *testing.T) {
	e := newEnv(t, opts{mutate: denyGuests})
	e.ms.SetRole(alice, "guest")
	e.send(e.activity("hello"))
	if _, ok := e.ms.WaitFor(5*time.Second, func(c testbot.Call) bool { return strings.Contains(c.Text(), "guests can't use this agent") }); !ok {
		t.Fatalf("the guest should be told they can't use the agent: %s", dump(e.ms.Calls()))
	}
	if len(e.agent.Requests()) != 0 {
		t.Fatal("a guest's message reached the agent")
	}
}

func TestMembersPassAndTheRoleIsCached(t *testing.T) {
	e := newEnv(t, opts{mutate: denyGuests})
	e.send(e.activity("first"))
	e.waitFinal("Echo: first")
	e.send(e.activity("second"))
	e.waitFinal("Echo: second")
	lookups := 0
	for _, c := range e.ms.Calls() {
		if c.Method == http.MethodGet && strings.Contains(c.Path, "/members/") {
			lookups++
		}
	}
	if lookups != 1 {
		t.Fatalf("the member role should be looked up once and cached, got %d lookups", lookups)
	}
}

func TestGuestCheckFailsClosed(t *testing.T) {
	e := newEnv(t, opts{mutate: denyGuests})
	e.ms.FailPath("/members/", http.StatusInternalServerError)
	e.send(e.activity("hello"))
	if _, ok := e.ms.WaitFor(5*time.Second, func(c testbot.Call) bool { return strings.Contains(c.Text(), "verify") }); !ok {
		t.Fatalf("a failed role lookup must deny access with a notice: %s", dump(e.ms.Calls()))
	}
	if len(e.agent.Requests()) != 0 {
		t.Fatal("the agent was called although the role could not be verified")
	}
}
