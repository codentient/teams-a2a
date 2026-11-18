package bridge

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// Security review M1: typed words never approve a tool call (only the card, whose nonce a
// forged button or a casual "ok" cannot know), and approvals expire.

func TestTypedWordsNeverApproveAToolCall(t *testing.T) {
	for _, word := range []string{"approve", "Approved.", "yes", "y", "OK", "ok!"} {
		t.Run(word, func(t *testing.T) {
			sink := &fakeSink{complete: true}
			b, ag, alice := pausedBridge(t, sink)
			_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Text: word}, sink)
			if len(ag.sent) != 1 {
				t.Fatalf("typing %q sent an answer to the agent", word)
			}
			if !strings.Contains(sink.lastNotice(), "**Approve** button") {
				t.Errorf("the user should be pointed to the card: %q", sink.lastNotice())
			}
			// The card still approves.
			_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Submission: submission(sink.prompts[0].Nonce, "approve")}, sink)
			if len(ag.sent) != 2 || !strings.Contains(hitlResponse(t, ag.sent[1]), "approved:true") {
				t.Fatal("the card's Approve button must still approve")
			}
		})
	}
}

func TestTypedRejectStillRejects(t *testing.T) {
	sink := &fakeSink{complete: true}
	b, ag, alice := pausedBridge(t, sink)
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Text: "reject"}, sink)
	if len(ag.sent) != 2 || !strings.Contains(hitlResponse(t, ag.sent[1]), "approved:false") {
		t.Fatal("typing reject must reject")
	}
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.now = c.now.Add(d); c.mu.Unlock() }

func TestExpiredApprovalIsRejectedNotApproved(t *testing.T) {
	for _, answer := range []string{"card approve", "typed text"} {
		t.Run(answer, func(t *testing.T) {
			clk := &clock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
			sink := &fakeSink{complete: true}
			b, ag, alice := pausedBridge(t, sink, func(o *Options) { o.ApprovalTTL = 30 * time.Minute; o.Now = clk.Now })
			clk.add(31 * time.Minute)
			turn := Turn{SessionKey: "s", User: alice, Submission: submission(sink.prompts[0].Nonce, "approve")}
			if answer == "typed text" {
				turn = Turn{SessionKey: "s", User: alice, Text: "is it done?"}
			}
			_ = b.Handle(context.Background(), turn, sink)
			if len(ag.sent) != 2 {
				t.Fatalf("the expired request must be answered with a rejection, sent=%d", len(ag.sent))
			}
			resp := hitlResponse(t, ag.sent[1])
			if !strings.Contains(resp, "approved:false") || !strings.Contains(resp, "expired") {
				t.Fatalf("expired approval must reach the agent as a rejection that says why: %s", resp)
			}
			if !strings.Contains(strings.Join(sink.notices, "|"), "expired after 30m") {
				t.Errorf("the user should be told the request expired: %q", sink.notices)
			}
			if len(sink.retired) != 1 || !strings.Contains(sink.retired[0], "Expired") {
				t.Errorf("the card should be retired as expired: %q", sink.retired)
			}
			// The old card cannot approve afterwards.
			_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Submission: submission(sink.prompts[0].Nonce, "approve")}, sink)
			for _, m := range ag.sent[2:] {
				if strings.Contains(hitlResponse(t, m), "approved:true") {
					t.Fatal("an expired card approved a tool call")
				}
			}
		})
	}
}

func TestApprovalWithinTTLStillWorks(t *testing.T) {
	clk := &clock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	sink := &fakeSink{complete: true}
	b, ag, alice := pausedBridge(t, sink, func(o *Options) { o.ApprovalTTL = 30 * time.Minute; o.Now = clk.Now })
	clk.add(29 * time.Minute)
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Submission: submission(sink.prompts[0].Nonce, "approve")}, sink)
	if len(ag.sent) != 2 || !strings.Contains(hitlResponse(t, ag.sent[1]), "approved:true") {
		t.Fatal("an approval within the TTL must work")
	}
}

func TestShortDuration(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Minute: "30m", 10 * time.Minute: "10m", time.Hour: "1h", 90 * time.Minute: "1h30m", 2 * time.Hour: "2h", 45 * time.Second: "1m",
	} {
		if got := shortDuration(d); got != want {
			t.Errorf("shortDuration(%v) = %q, want %q", d, got, want)
		}
	}
}
