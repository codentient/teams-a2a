package teams

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/codentient/teams-a2a/internal/bridge"
	"github.com/codentient/teams-a2a/internal/store"
)

func cardJSON(t *testing.T, c card) string {
	t.Helper()
	b, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func actionsOf(c card) []string {
	var out []string
	acts, _ := c["actions"].([]any)
	for _, a := range acts {
		data, _ := a.(map[string]any)["data"].(map[string]any)
		meta, _ := data[bridge.SubmitKey].(map[string]any)
		out = append(out, fmt.Sprint(meta["action"]))
	}
	return out
}

// Every tool an approval covers must be on the card with its complete arguments.
func TestApprovalCardShowsEveryToolInFull(t *testing.T) {
	var tools []bridge.PromptTool
	for i := 0; i < 10; i++ {
		tools = append(tools, bridge.PromptTool{Name: "get_pods", Args: map[string]any{"ns": "default"}})
	}
	tools = append(tools,
		bridge.PromptTool{Name: "delete_namespace", Args: map[string]any{"name": "production"}},
		bridge.PromptTool{Name: "kubectl_apply", Args: map[string]any{
			"a_description": strings.Repeat(" ", 1600),
			"z_manifest":    "kind: ClusterRoleBinding cluster-admin attacker",
		}},
	)
	c, complete := PromptCard(bridge.Prompt{Kind: store.PendingToolApproval, Nonce: "n", Tools: tools}, false)
	if !complete {
		t.Fatal("card unexpectedly reported as incomplete")
	}
	s := cardJSON(t, c)
	for _, tool := range tools {
		if !strings.Contains(s, tool.Name) {
			t.Errorf("tool %s missing from card", tool.Name)
		}
		args, _ := json.Marshal(tool.Args)
		for _, v := range tool.Args {
			if !strings.Contains(s, fmt.Sprint(v)) {
				t.Errorf("argument %s of %s missing from card", args, tool.Name)
			}
		}
	}
	if strings.Count(s, `"text":"get_pods"`) != 10 {
		t.Errorf("expected all 10 get_pods calls on the card")
	}
	if got := actionsOf(c); fmt.Sprint(got) != "[approve reject]" {
		t.Errorf("actions = %v", got)
	}
}

func TestAgentHintDoesNotReplaceGatewayIntro(t *testing.T) {
	c, _ := PromptCard(bridge.Prompt{Kind: store.PendingToolApproval, Nonce: "n", Hint: "Approve to list pods (read-only).",
		Tools: []bridge.PromptTool{{Name: "delete_namespace"}}}, false)
	s := cardJSON(t, c)
	for _, want := range []string{"The agent wants to run the following tool:", "Agent's explanation:", "Approve to list pods (read-only)."} {
		if !strings.Contains(s, want) {
			t.Errorf("card is missing %q", want)
		}
	}
}

func TestOversizedApprovalCannotBeApprovedFromCard(t *testing.T) {
	var tools []bridge.PromptTool
	for i := 0; i < 50; i++ {
		tools = append(tools, bridge.PromptTool{Name: fmt.Sprintf("tool_%d", i), Args: map[string]any{"blob": strings.Repeat("x", 1000)}})
	}
	c, complete := PromptCard(bridge.Prompt{Kind: store.PendingToolApproval, Nonce: "n", OwnerID: "alice", Tools: tools}, false)
	if complete {
		t.Fatal("oversized card reported as complete")
	}
	if got := actionsOf(c); fmt.Sprint(got) != "[reject]" {
		t.Errorf("oversized card must only offer reject, got %v", got)
	}
	s := cardJSON(t, c)
	if len(s) > maxCardBytes || !strings.Contains(s, "50 tool call(s)") || strings.Contains(s, "tool_0") {
		t.Errorf("unexpected fallback card (%d bytes): %.200s", len(s), s)
	}
}

func TestOversizedQuestionHasNoActions(t *testing.T) {
	q := store.Question{Text: strings.Repeat("why? ", 6000)}
	c, complete := PromptCard(bridge.Prompt{Kind: store.PendingAskUser, Nonce: "n", Questions: []store.Question{q}}, false)
	if complete || len(actionsOf(c)) != 0 {
		t.Fatalf("complete=%v actions=%v", complete, actionsOf(c))
	}
}
