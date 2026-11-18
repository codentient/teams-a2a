package bridge

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/codentient/teams-a2a/internal/identity"
	"github.com/codentient/teams-a2a/internal/store"
)

// Security review L2: streamed answers must cost linear time, not quadratic.
func TestAccumulatorIsLinearInStreamedChunks(t *testing.T) {
	run := func(n int) time.Duration {
		acc := newAccumulator()
		start := time.Now()
		var id a2a.ArtifactID = "a"
		for i := range n {
			acc.apply(&a2a.TaskArtifactUpdateEvent{TaskID: "t", ContextID: "c", Append: i > 0,
				Artifact: &a2a.Artifact{ID: id, Parts: []*a2a.Part{a2a.NewTextPart(strings.Repeat("x", 16))}}})
		}
		if got := len(acc.text()); got != n*16 {
			t.Fatalf("text length %d, want %d", got, n*16)
		}
		return time.Since(start)
	}
	run(2_000) // warm up
	small, large := run(10_000), run(40_000)
	if ratio := float64(large) / float64(small); ratio > 8 {
		t.Fatalf("4x the chunks took %.1fx the time (%v vs %v); expected roughly linear", ratio, small, large)
	}
}

// Merging must keep the observable behaviour: chunks merge, thoughts stay separate.
func TestAccumulatorMergeKeepsThoughtsSeparate(t *testing.T) {
	acc := newAccumulator()
	first := a2a.NewArtifactEvent(info, a2a.NewTextPart("a"))
	acc.apply(first)
	th := a2a.NewTextPart("thinking")
	th.SetMeta(thoughtKey, true)
	acc.apply(a2a.NewArtifactUpdateEvent(info, first.Artifact.ID, a2a.NewTextPart("b")))
	acc.apply(a2a.NewArtifactUpdateEvent(info, first.Artifact.ID, th))
	acc.apply(a2a.NewArtifactUpdateEvent(info, first.Artifact.ID, a2a.NewTextPart("c")))
	acc.apply(a2a.NewArtifactUpdateEvent(info, first.Artifact.ID, a2a.NewTextPart("d")))
	if got := acc.text(); got != "ab\n\ncd" {
		t.Fatalf("text = %q, want %q", got, "ab\n\ncd")
	}
}

type countingSink struct {
	fakeSink
	mu       sync.Mutex
	partials int
}

func (s *countingSink) Partial(string) { s.mu.Lock(); s.partials++; s.mu.Unlock() }

type chunkAgent struct {
	fakeAgent
	n, size int
}

func (a *chunkAgent) Stream(_ context.Context, m *a2a.Message) iter.Seq2[a2a.Event, error] {
	return func(yield func(a2a.Event, error) bool) {
		info := a2a.TaskInfo{TaskID: "t1", ContextID: "c1"}
		first := a2a.NewArtifactEvent(info, a2a.NewTextPart(strings.Repeat("y", a.size)))
		if !yield(first, nil) {
			return
		}
		for range a.n - 1 {
			if !yield(a2a.NewArtifactUpdateEvent(info, first.Artifact.ID, a2a.NewTextPart(strings.Repeat("y", a.size))), nil) {
				return
			}
		}
		yield(&a2a.TaskStatusUpdateEvent{TaskID: "t1", ContextID: "c1", Status: a2a.TaskStatus{State: a2a.TaskStateCompleted}}, nil)
	}
}

func TestPartialUpdatesAreThrottled(t *testing.T) {
	ag := &chunkAgent{n: 5000, size: 8}
	b := New(Options{Store: store.NewMemory(time.Hour, 10), Agent: ag, TurnTimeout: time.Minute})
	sink := &countingSink{}
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: identity.User{ID: "u"}, Text: "go"}, sink)
	if sink.partials > 50 {
		t.Fatalf("5000 chunks produced %d partial renders; they should be throttled", sink.partials)
	}
	if len(sink.finals) != 1 || len(sink.finals[0].Text) != 5000*8 {
		t.Fatalf("the final answer must hold the whole text, got %d finals", len(sink.finals))
	}
}

func TestOversizedAnswerIsStopped(t *testing.T) {
	ag := &chunkAgent{n: 100, size: 16 << 10} // 1.6 MiB
	b := New(Options{Store: store.NewMemory(time.Hour, 10), Agent: ag, TurnTimeout: time.Minute})
	sink := &countingSink{}
	_ = b.Handle(context.Background(), Turn{SessionKey: "s", User: identity.User{ID: "u"}, Text: "go"}, sink)
	if len(ag.canceled) != 1 || ag.canceled[0] != "t1" {
		t.Fatalf("the agent task should be cancelled when the answer exceeds the limit, canceled=%v", ag.canceled)
	}
	if len(sink.finals) != 1 || !sink.finals[0].IsError || !strings.Contains(sink.finals[0].Text, "too long") {
		t.Fatalf("the user should be told the answer was too long")
	}
	if n := len(sink.finals[0].Text); n > 64<<10 {
		t.Fatalf("the final message must be bounded, got %d bytes", n)
	}
}
