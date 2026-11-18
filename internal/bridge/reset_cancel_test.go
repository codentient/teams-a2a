package bridge

import (
	"context"
	"iter"
	"strings"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/codentient/teams-a2a/internal/identity"
	"github.com/codentient/teams-a2a/internal/store"
)

// Security review M3: with approvalPolicy requester, only the person who started a request may
// cancel it or discard it with /new.

var mallory = identity.User{ID: "mallory", Name: "Mallory"}

func TestOthersCannotDiscardSomeoneElsesPendingRequest(t *testing.T) {
	sink := &fakeSink{complete: true}
	b, _, alice := pausedBridge(t, sink)
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: mallory, Text: "/new"}, sink)
	if !strings.Contains(sink.lastNotice(), "Only Alice") {
		t.Fatalf("mallory should be told only Alice can do this: %q", sink.lastNotice())
	}
	sess, _ := b.o.Store.GetSession(context.Background(), "s")
	if sess == nil || sess.Pending == nil {
		t.Fatal("mallory's /new discarded Alice's pending request")
	}
	if len(sink.retired) != 0 {
		t.Fatal("Alice's card must not be retired")
	}
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Text: "/new"}, sink)
	if sess, _ := b.o.Store.GetSession(context.Background(), "s"); sess != nil {
		t.Fatal("Alice's own /new must start a new conversation")
	}
}

func TestOthersCannotCancelSomeoneElsesRunningTask(t *testing.T) {
	ag := &fakeAgent{}
	st := store.NewMemory(time.Hour, 100)
	b := New(Options{Store: st, Agent: ag, TurnTimeout: time.Minute, ApprovalPolicy: "requester"})
	sink := &fakeSink{}
	_ = st.PutSession(context.Background(), "s", &store.Session{RunningTaskID: "alice-task", RunningBy: "alice", RunningByName: "Alice"})
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: mallory, Text: "/cancel"}, sink)
	if len(ag.canceled) != 0 {
		t.Fatalf("mallory cancelled Alice's task: %v", ag.canceled)
	}
	if !strings.Contains(sink.lastNotice(), "Only Alice") {
		t.Errorf("unexpected notice %q", sink.lastNotice())
	}
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: identity.User{ID: "alice", Name: "Alice"}, Text: "/cancel"}, sink)
	if len(ag.canceled) != 1 || ag.canceled[0] != "alice-task" {
		t.Fatalf("Alice must be able to cancel her own task: %v", ag.canceled)
	}
}

func TestAnyonePolicyLetsMembersResetAndCancel(t *testing.T) {
	sink := &fakeSink{complete: true}
	b, _, _ := pausedBridge(t, sink, func(o *Options) { o.ApprovalPolicy = "anyone" })
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: mallory, Text: "/new"}, sink)
	if sess, _ := b.o.Store.GetSession(context.Background(), "s"); sess != nil {
		t.Fatal("with approvalPolicy anyone, any member may start a new conversation")
	}
	ag := &fakeAgent{}
	st := store.NewMemory(time.Hour, 100)
	b2 := New(Options{Store: st, Agent: ag, TurnTimeout: time.Minute, ApprovalPolicy: "anyone"})
	_ = st.PutSession(context.Background(), "s", &store.Session{RunningTaskID: "alice-task", RunningBy: "alice"})
	_ = b2.Handle(context.Background(), Turn{SessionKey: "s", User: mallory, Text: "/cancel"}, sink)
	if len(ag.canceled) != 1 {
		t.Fatal("with approvalPolicy anyone, any member may cancel")
	}
}

func TestStalePendingRequestCanBeDiscardedByOthers(t *testing.T) {
	clk := &clock{now: time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)}
	sink := &fakeSink{complete: true}
	b, _, _ := pausedBridge(t, sink, func(o *Options) { o.ApprovalTTL = 30 * time.Minute; o.Now = clk.Now })
	clk.add(31 * time.Minute)
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: mallory, Text: "/new"}, sink)
	if sess, _ := b.o.Store.GetSession(context.Background(), "s"); sess != nil {
		t.Fatal("a request older than approvalTTL must not lock the chat for others")
	}
}

// observingAgent reads the stored session after its first event has been applied.
type observingAgent struct {
	fakeAgent
	st   store.Store
	seen *store.Session
}

func (a *observingAgent) Stream(ctx context.Context, m *a2a.Message) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		if !yield(&a2a.TaskStatusUpdateEvent{TaskID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateWorking}}, nil) {
			return
		}
		a.seen, _ = a.st.GetSession(ctx, "s")
		yield(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("done")), nil)
	}
}

func TestRunningTaskRecordsWhoStartedIt(t *testing.T) {
	st := store.NewMemory(time.Hour, 100)
	ag := &observingAgent{st: st}
	b := New(Options{Store: st, Agent: ag, TurnTimeout: time.Minute, ApprovalPolicy: "requester"})
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: identity.User{ID: "alice", Name: "Alice"}, Text: "restart web"}, &fakeSink{})
	if ag.seen == nil || ag.seen.RunningTaskID != "t1" || ag.seen.RunningBy != "alice" || ag.seen.RunningByName != "Alice" {
		t.Fatalf("running task must record who started it: %+v", ag.seen)
	}
	if after, _ := st.GetSession(context.Background(), "s"); after.RunningTaskID != "" || after.RunningBy != "" {
		t.Fatalf("finished task must clear the running fields: %+v", after)
	}
}
