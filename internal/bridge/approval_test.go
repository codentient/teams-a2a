package bridge

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/codentient/teams-a2a/internal/agent"
	"github.com/codentient/teams-a2a/internal/identity"
	"github.com/codentient/teams-a2a/internal/store"
)

type fakeAgent struct {
	mu       sync.Mutex
	sent     []*a2a.Message
	canceled []string
	events   func(msg *a2a.Message) []a2a.Event
}

func (f *fakeAgent) Card(context.Context) (*a2a.AgentCard, error) { return &a2a.AgentCard{}, nil }
func (f *fakeAgent) HITL(*a2a.AgentCard) bool                     { return true }
func (f *fakeAgent) GetTask(context.Context, string) (*a2a.Task, error) {
	return nil, fmt.Errorf("not implemented")
}
func (f *fakeAgent) Cancel(_ context.Context, id string) error {
	f.mu.Lock()
	f.canceled = append(f.canceled, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeAgent) Stream(_ context.Context, m *a2a.Message) iter.Seq2[a2a.Event, error] {
	f.mu.Lock()
	f.sent = append(f.sent, m)
	f.mu.Unlock()
	return func(yield func(a2a.Event, error) bool) {
		for _, e := range f.events(m) {
			if !yield(e, nil) {
				return
			}
		}
	}
}

type fakeSink struct {
	mu       sync.Mutex
	complete bool
	failSend bool
	prompts  []Prompt
	finals   []Reply
	notices  []string
}

func (s *fakeSink) Progress(string) {}
func (s *fakeSink) Partial(string)  {}
func (s *fakeSink) Final(_ context.Context, r Reply) error {
	s.mu.Lock()
	s.finals = append(s.finals, r)
	s.mu.Unlock()
	return nil
}
func (s *fakeSink) Notice(_ context.Context, t string) error {
	s.mu.Lock()
	s.notices = append(s.notices, t)
	s.mu.Unlock()
	return nil
}
func (s *fakeSink) Prompt(_ context.Context, p Prompt) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prompts = append(s.prompts, p)
	if s.failSend {
		return "", s.complete, fmt.Errorf("send failed")
	}
	return "card-1", s.complete, nil
}
func (s *fakeSink) Retire(context.Context, string, string) error { return nil }
func (s *fakeSink) lastNotice() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.notices) == 0 {
		return ""
	}
	return s.notices[len(s.notices)-1]
}

func approvalPause(tools ...map[string]any) []a2a.Event {
	status := a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("approval needed"))
	status.Extensions = []string{agent.HITLExtensionURI}
	ts := make([]any, len(tools))
	for i, t := range tools {
		ts[i] = t
	}
	status.Metadata = map[string]any{agent.HITLExtensionURI: map[string]any{"type": "tool_approval_request", "tools": ts}}
	return []a2a.Event{&a2a.TaskStatusUpdateEvent{TaskID: "t1", ContextID: "c1",
		Status: a2a.TaskStatus{State: a2a.TaskStateInputRequired, Message: status}}}
}

// pausedBridge runs one turn that ends in a tool approval request and returns the pieces.
func pausedBridge(t *testing.T, sink *fakeSink) (*Bridge, *fakeAgent, identity.User) {
	t.Helper()
	ag := &fakeAgent{}
	first := true
	ag.events = func(*a2a.Message) []a2a.Event {
		if first {
			first = false
			return approvalPause(map[string]any{"id": "call-1", "name": "delete_namespace", "args": map[string]any{"name": "prod"}})
		}
		return []a2a.Event{a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("done"))}
	}
	b := New(Options{Store: store.NewMemory(time.Hour, 100), Agent: ag, TurnTimeout: time.Minute, ApprovalPolicy: "requester"})
	alice := identity.User{ID: "alice", Name: "Alice"}
	if err := b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Text: "clean up"}, sink); err != nil {
		t.Fatal(err)
	}
	if len(sink.prompts) != 1 {
		t.Fatalf("expected one prompt, got %d", len(sink.prompts))
	}
	return b, ag, alice
}

func submission(nonce, action string) map[string]any {
	return map[string]any{SubmitKey: map[string]any{"nonce": nonce, "owner": "alice", "action": action}}
}

func hitlResponse(t *testing.T, m *a2a.Message) string {
	t.Helper()
	return fmt.Sprint(m.Metadata[agent.HITLExtensionURI])
}

func TestApprovalAllowedWhenRequestShownInFull(t *testing.T) {
	sink := &fakeSink{complete: true}
	b, ag, alice := pausedBridge(t, sink)
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Text: "approve"}, sink)
	if len(ag.sent) != 2 || !strings.Contains(hitlResponse(t, ag.sent[1]), "approved:true") {
		t.Fatalf("expected the approval to reach the agent, sent=%d", len(ag.sent))
	}
}

func TestIncompleteRequestCannotBeApproved(t *testing.T) {
	for _, tc := range []struct {
		name string
		sink *fakeSink
	}{
		{"card too large", &fakeSink{complete: false}},
		{"card failed to send", &fakeSink{complete: true, failSend: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, ag, alice := pausedBridge(t, tc.sink)
			nonce := tc.sink.prompts[0].Nonce

			_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Text: "approve"}, tc.sink)
			_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Submission: submission(nonce, "approve")}, tc.sink)
			if len(ag.sent) != 1 {
				t.Fatalf("approval of an incomplete request reached the agent")
			}
			if !strings.Contains(tc.sink.lastNotice(), "can't be approved") {
				t.Fatalf("unexpected notice %q", tc.sink.lastNotice())
			}

			_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: alice, Submission: submission(nonce, "reject")}, tc.sink)
			if len(ag.sent) != 2 || !strings.Contains(hitlResponse(t, ag.sent[1]), "approved:false") {
				t.Fatalf("expected the rejection to reach the agent")
			}
		})
	}
}
