package gateway_test

import (
	"strings"
	"testing"
	"time"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/testbot"
)

// Security review L1: one user cannot hold more than maxTurnsPerUser of the gateway's slots.
func TestPerUserTurnCap(t *testing.T) {
	e := newEnv(t, opts{mutate: func(c *config.Config) { c.Server.MaxTurnsPerUser = 3 }})
	e.agent.ChunkDelay = 700 * time.Millisecond // each turn runs for a few seconds
	for i := range 4 {
		chat := func(m map[string]any) {
			m["conversation"] = map[string]any{"id": "a:chat-" + string(rune('a'+i)), "conversationType": "personal", "tenantId": tenantID}
		}
		e.send(e.activity("long job", chat))
	}
	if _, ok := e.ms.WaitFor(5*time.Second, func(c testbot.Call) bool { return strings.Contains(c.Text(), "already have 3 requests running") }); !ok {
		t.Fatalf("the 4th concurrent turn should be refused: %s", dump(e.ms.Calls()))
	}
	// The three admitted turns still complete.
	done := 0
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && done < 3 {
		done = 0
		for _, c := range e.ms.Calls() {
			if c.Type() == "message" && strings.Contains(c.Text(), "Echo: long job") {
				done++
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	if done != 3 {
		t.Fatalf("want 3 completed turns, got %d", done)
	}
}
