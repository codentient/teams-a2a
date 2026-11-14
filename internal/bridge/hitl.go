package bridge

import (
	"encoding/json"
	"slices"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/codentient/teams-a2a/internal/agent"
)

// Wire types of kagent's HITL extension (https://kagent.dev/extensions/hitl/v1), mirrored
// from kagent's go/api/a2a/hitl.go.

type hitlTool struct {
	ID     string         `json:"id"`
	CallID string         `json:"call_id,omitempty"`
	Name   string         `json:"name"`
	Args   map[string]any `json:"args,omitempty"`
}

type hitlNested struct {
	SubagentName string     `json:"subagent_name,omitempty"`
	TaskID       string     `json:"task_id,omitempty"`
	ContextID    string     `json:"context_id,omitempty"`
	Tools        []hitlTool `json:"tools"`
}

type hitlQuestion struct {
	Question string   `json:"question"`
	Choices  []string `json:"choices"`
	Multiple bool     `json:"multiple"`
}

type hitlRequest struct {
	Type      string         `json:"type"`
	ID        string         `json:"id,omitempty"`
	Hint      string         `json:"hint,omitempty"`
	Tools     []hitlTool     `json:"tools,omitempty"`
	Questions []hitlQuestion `json:"questions,omitempty"`
	Nested    *hitlNested    `json:"nested,omitempty"`
}

// tools returns the calls the person must decide on. For a nested pause these are the
// subagent's tools, as kagent validates the response against them.
func (r *hitlRequest) tools() []hitlTool {
	if r.Nested != nil && len(r.Nested.Tools) > 0 {
		return r.Nested.Tools
	}
	return r.Tools
}

type toolApproval struct {
	ID              string `json:"id"`
	Approved        bool   `json:"approved"`
	RejectionReason string `json:"rejection_reason,omitempty"`
}

type askUserAnswer struct {
	Answer []string `json:"answer"`
}

const (
	typeToolApprovalRequest  = "tool_approval_request"
	typeAskUserRequest       = "ask_user_request"
	typeToolApprovalResponse = "tool_approval_response"
	typeAskUserResponse      = "ask_user_response"
)

// parseHITL extracts a structured pause from a status message, if the agent sent one.
func parseHITL(m *a2a.Message) *hitlRequest {
	if m == nil || m.Metadata == nil || !slices.Contains(m.Extensions, agent.HITLExtensionURI) {
		return nil
	}
	raw, ok := m.Metadata[agent.HITLExtensionURI]
	if !ok {
		return nil
	}
	b, err := json.Marshal(raw)
	if err != nil {
		return nil
	}
	var req hitlRequest
	if json.Unmarshal(b, &req) != nil {
		return nil
	}
	switch req.Type {
	case typeToolApprovalRequest:
		if len(req.tools()) == 0 {
			return nil
		}
	case typeAskUserRequest:
		if req.ID == "" || len(req.Questions) == 0 {
			return nil
		}
	default:
		return nil
	}
	return &req
}

func toolApprovalMessage(taskID, contextID string, toolIDs []string, approved bool, reason, text string) *a2a.Message {
	approvals := make([]toolApproval, 0, len(toolIDs))
	for _, id := range toolIDs {
		a := toolApproval{ID: id, Approved: approved}
		if !approved {
			a.RejectionReason = reason
		}
		approvals = append(approvals, a)
	}
	return hitlMessage(taskID, contextID, text, map[string]any{
		"type":      typeToolApprovalResponse,
		"approvals": approvals,
	})
}

func askUserMessage(taskID, contextID, requestID string, answers [][]string, text string) *a2a.Message {
	out := make([]askUserAnswer, len(answers))
	for i, a := range answers {
		out[i] = askUserAnswer{Answer: a}
	}
	return hitlMessage(taskID, contextID, text, map[string]any{
		"type":    typeAskUserResponse,
		"id":      requestID,
		"answers": out,
	})
}

func hitlMessage(taskID, contextID, text string, payload map[string]any) *a2a.Message {
	// Round-trip through JSON so the metadata holds plain maps, like kagent's own AttachHITL.
	b, _ := json.Marshal(payload)
	var raw map[string]any
	_ = json.Unmarshal(b, &raw)
	m := a2a.NewMessageForTask(a2a.MessageRoleUser, a2a.TaskInfo{TaskID: a2a.TaskID(taskID), ContextID: contextID}, a2a.NewTextPart(text))
	m.Extensions = []string{agent.HITLExtensionURI}
	m.Metadata = map[string]any{agent.HITLExtensionURI: raw}
	return m
}
