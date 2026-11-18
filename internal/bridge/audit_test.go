package bridge

import (
	"bytes"
	"context"
	"github.com/codentient/teams-a2a/internal/identity"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codentient/teams-a2a/internal/store"
)

type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) auditLines() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range strings.Split(l.b.String(), "\n") {
		if strings.Contains(line, "audit=true") {
			out = append(out, line)
		}
	}
	return out
}

// Security review L5: every HITL decision, /cancel and /new leaves a structured audit record.
func TestDecisionsAreAudited(t *testing.T) {
	cases := map[string]struct {
		act  func(b *Bridge, sink *fakeSink)
		want []string
	}{
		"card approval": {func(b *Bridge, sink *fakeSink) {
			_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: aliceUser, Submission: submission(sink.prompts[0].Nonce, "approve")}, sink)
		}, []string{"event=tool_approval", "decision=approved", "user=alice", "task=t1", "tools=delete_namespace"}},
		"typed rejection": {func(b *Bridge, sink *fakeSink) {
			_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: aliceUser, Text: "reject"}, sink)
		}, []string{"event=tool_approval", "decision=rejected", "user=alice"}},
		"new conversation": {func(b *Bridge, sink *fakeSink) {
			_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: aliceUser, Text: "/new"}, sink)
		}, []string{"event=new_conversation", "user=alice"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			var logs logBuf
			sink := &fakeSink{complete: true}
			b, _, _ := pausedBridge(t, sink, func(o *Options) { o.Logger = slog.New(slog.NewTextHandler(&logs, nil)) })
			tc.act(b, sink)
			lines := logs.auditLines()
			if len(lines) != 1 {
				t.Fatalf("want exactly one audit record, got %q", lines)
			}
			for _, w := range tc.want {
				if !strings.Contains(lines[0], w) {
					t.Errorf("audit record %q lacks %q", lines[0], w)
				}
			}
		})
	}
}

func TestExpiryAndCancelAreAudited(t *testing.T) {
	var logs logBuf
	clk := &clock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	sink := &fakeSink{complete: true}
	b, _, _ := pausedBridge(t, sink, func(o *Options) {
		o.Logger = slog.New(slog.NewTextHandler(&logs, nil))
		o.Now = clk.Now
	})
	clk.add(31 * time.Minute)
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: aliceUser, Text: "hello?"}, sink)
	if l := logs.auditLines(); len(l) != 1 || !strings.Contains(l[0], "decision=expired") {
		t.Fatalf("expiry not audited: %q", l)
	}
	_ = b.o.Store.PutSession(context.Background(), "s", &store.Session{RunningTaskID: "t9", RunningBy: "alice", RunningByName: "Alice"})
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: aliceUser, Text: "/cancel"}, sink)
	if l := logs.auditLines(); len(l) != 2 || !strings.Contains(l[1], "event=cancel") || !strings.Contains(l[1], "task=t9") {
		t.Fatalf("cancel not audited: %q", l)
	}
}

var aliceUser = identity.User{ID: "alice", Name: "Alice", TenantID: "tenant-1"}
