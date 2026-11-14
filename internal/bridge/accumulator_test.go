package bridge

import (
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

var info = a2a.TaskInfo{TaskID: "t1", ContextID: "c1"}

func TestAccumulatorMergesStreamedChunks(t *testing.T) {
	acc := newAccumulator()
	first := a2a.NewArtifactEvent(info, a2a.NewTextPart("1"))
	acc.apply(first)
	for _, c := range []string{",", " 2", ", 3"} {
		if u := acc.apply(a2a.NewArtifactUpdateEvent(info, first.Artifact.ID, a2a.NewTextPart(c))); !u.textChanged {
			t.Fatal("each chunk should change the text")
		}
	}
	if got := acc.text(); got != "1, 2, 3" {
		t.Errorf("text = %q", got)
	}
	// A non-append event with the same id replaces the artifact (final full snapshot).
	acc.apply(&a2a.TaskArtifactUpdateEvent{TaskID: "t1", ContextID: "c1", Artifact: &a2a.Artifact{ID: first.Artifact.ID, Parts: a2a.ContentParts{a2a.NewTextPart("final")}}})
	if got := acc.text(); got != "final" {
		t.Errorf("replace: text = %q", got)
	}
	if acc.taskID != "t1" || acc.contextID != "c1" {
		t.Errorf("ids not tracked")
	}
}

func TestToolActivityIsProgressNotOutput(t *testing.T) {
	acc := newAccumulator()
	call := a2a.NewDataPart(map[string]any{"id": "1", "name": "k8s_get_pods"})
	call.SetMeta(partTypeKey, "function_call")
	u := acc.apply(a2a.NewStatusUpdateEvent(info, a2a.TaskStateWorking, a2a.NewMessageForTask(a2a.MessageRoleAgent, info, call)))
	if len(u.progress) != 1 || u.progress[0] != "Running `k8s_get_pods`…" {
		t.Errorf("progress = %v", u.progress)
	}
	thought := a2a.NewTextPart("let me think")
	thought.SetMeta(thoughtKey, true)
	acc.apply(a2a.NewArtifactEvent(info, thought, call, a2a.NewTextPart("Answer")))
	if got := acc.text(); got != "Answer" {
		t.Errorf("thoughts and tool calls must not leak into the answer, got %q", got)
	}
}

func TestReplyRendersNonTextParts(t *testing.T) {
	acc := newAccumulator()
	card := a2a.NewDataPart(map[string]any{"type": "AdaptiveCard"})
	card.MediaType = adaptiveCard
	data := a2a.NewDataPart(map[string]any{"replicas": 3})
	link := a2a.NewFileURLPart("https://example.com/report.pdf", "application/pdf")
	link.Filename = "report [final].pdf"
	unsafe := a2a.NewFileURLPart("javascript:alert(1)", "text/html")
	img := a2a.NewRawPart([]byte{0x89, 'P', 'N', 'G'})
	img.MediaType = "image/png"
	acc.apply(a2a.NewArtifactEvent(info, a2a.NewTextPart("Here you go"), card, data, link, unsafe, img))
	acc.apply(a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, nil))

	r := acc.reply()
	if len(r.Cards) != 1 || len(r.Images) != 1 || !strings.HasPrefix(r.Images[0].DataURL, "data:image/png;base64,") {
		t.Errorf("cards/images not extracted: %+v", r)
	}
	for _, want := range []string{"Here you go", "\"replicas\": 3", "[report (final).pdf](https://example.com/report.pdf)"} {
		if !strings.Contains(r.Text, want) {
			t.Errorf("reply missing %q:\n%s", want, r.Text)
		}
	}
	if strings.Contains(r.Text, "javascript:") {
		t.Error("non-https links must be dropped")
	}
}

func TestDirectMessageAndStatusFallback(t *testing.T) {
	acc := newAccumulator()
	acc.apply(a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("direct")))
	if acc.text() != "direct" || acc.state != a2a.TaskStateCompleted {
		t.Errorf("direct message: %q %s", acc.text(), acc.state)
	}

	acc = newAccumulator()
	acc.apply(a2a.NewStatusUpdateEvent(info, a2a.TaskStateCompleted, a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.NewTextPart("in status"))))
	if acc.text() != "in status" {
		t.Errorf("answers carried in the completed status message must be shown, got %q", acc.text())
	}
}

func TestParseHITLRequiresDeclaredExtension(t *testing.T) {
	payload := map[string]any{"type": "ask_user_request", "id": "r1", "questions": []any{map[string]any{"question": "Which?"}}}
	m := a2a.NewMessageForTask(a2a.MessageRoleAgent, info, a2a.NewTextPart("Which?"))
	m.Metadata = map[string]any{"https://kagent.dev/extensions/hitl/v1": payload}
	if parseHITL(m) != nil {
		t.Error("payload without the extension declared must be ignored, as kagent does")
	}
	m.Extensions = []string{"https://kagent.dev/extensions/hitl/v1"}
	if r := parseHITL(m); r == nil || r.ID != "r1" || r.Questions[0].Question != "Which?" {
		t.Errorf("parse: %+v", r)
	}

	nested := map[string]any{"type": "tool_approval_request", "tools": []any{map[string]any{"id": "outer"}},
		"nested": map[string]any{"subagent_name": "child", "tools": []any{map[string]any{"id": "inner", "name": "rm"}}}}
	m.Metadata["https://kagent.dev/extensions/hitl/v1"] = nested
	if r := parseHITL(m); r == nil || r.tools()[0].ID != "inner" {
		t.Errorf("nested pauses must be decided on the subagent's tools: %+v", r)
	}
}
