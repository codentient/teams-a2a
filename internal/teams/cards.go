package teams

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/codentient/teams-a2a/internal/bridge"
	"github.com/codentient/teams-a2a/internal/store"
)

// maxCardBytes keeps a prompt card, plus the activity envelope around it, under the ~28 KB
// Teams message limit.
const maxCardBytes = 24 << 10

type card = map[string]any

func newCard(body []any, actions []any) card {
	c := card{
		"type":    "AdaptiveCard",
		"$schema": "http://adaptivecards.io/schemas/adaptive-card.json",
		"version": "1.5",
		"body":    body,
	}
	if len(actions) > 0 {
		c["actions"] = actions
	}
	return c
}

func textBlock(text string, opts ...func(map[string]any)) map[string]any {
	b := map[string]any{"type": "TextBlock", "text": text, "wrap": true}
	for _, o := range opts {
		o(b)
	}
	return b
}

func bold(b map[string]any)   { b["weight"] = "Bolder" }
func subtle(b map[string]any) { b["isSubtle"] = true; b["size"] = "Small" }
func heading(b map[string]any) {
	b["weight"] = "Bolder"
	b["size"] = "Medium"
}
func mono(b map[string]any) { b["fontType"] = "Monospace"; b["size"] = "Small" }

func submit(title, style, nonce, owner, action string) map[string]any {
	a := map[string]any{
		"type":  "Action.Submit",
		"title": title,
		"data":  map[string]any{bridge.SubmitKey: map[string]any{"nonce": nonce, "owner": owner, "action": action}},
	}
	if style != "" {
		a["style"] = style
	}
	return a
}

// PromptCard renders a human-in-the-loop request. complete reports whether the card shows
// the whole request. When the full card would not fit in a Teams message, a fallback card is
// returned instead: it never offers Approve, because the person could not see everything an
// approval would cover.
func PromptCard(p bridge.Prompt, requesterOnly bool) (c card, complete bool) {
	full := fullPromptCard(p, requesterOnly)
	if b, err := json.Marshal(full); err == nil && len(b) <= maxCardBytes {
		return full, true
	}
	return tooLargeCard(p), false
}

func fullPromptCard(p bridge.Prompt, requesterOnly bool) card {
	var body []any
	var actions []any
	who := ""
	if requesterOnly && p.RequesterName != "" {
		who = fmt.Sprintf("Only %s can answer this.", p.RequesterName)
	}
	switch p.Kind {
	case store.PendingToolApproval:
		body = append(body, textBlock("Approval needed", heading))
		intro := "The agent wants to run the following tool"
		if len(p.Tools) > 1 {
			intro += "s"
		}
		body = append(body, textBlock(intro+":"))
		if p.Hint != "" {
			body = append(body, textBlock("Agent's explanation:", subtle), textBlock(p.Hint))
		}
		if p.Subagent != "" {
			body = append(body, textBlock("Requested by sub-agent **"+p.Subagent+"**", subtle))
		}
		for _, t := range p.Tools {
			body = append(body, textBlock(t.Name, bold))
			if len(t.Args) > 0 {
				b, _ := json.MarshalIndent(t.Args, "", "  ")
				body = append(body, textBlock(string(b), mono))
			}
		}
		body = append(body, map[string]any{
			"type": "Input.Text", "id": "reason", "isMultiline": true, "maxLength": 500,
			"placeholder": "Reason (optional; sent to the agent if you reject)",
		})
		actions = []any{
			submit("Approve", "positive", p.Nonce, p.OwnerID, "approve"),
			submit("Reject", "destructive", p.Nonce, p.OwnerID, "reject"),
		}
	case store.PendingAskUser:
		body = append(body, textBlock("The agent has a question", heading))
		if p.Subagent != "" {
			body = append(body, textBlock("Asked by sub-agent **"+p.Subagent+"**", subtle))
		}
		for i, q := range p.Questions {
			id := "q" + strconv.Itoa(i)
			body = append(body, textBlock(q.Text, bold))
			if len(q.Choices) > 0 {
				choices := make([]any, len(q.Choices))
				for j, c := range q.Choices {
					// Values are indexes, so commas in choice text cannot break multi-select parsing.
					choices[j] = map[string]any{"title": c, "value": strconv.Itoa(j)}
				}
				body = append(body, map[string]any{
					"type": "Input.ChoiceSet", "id": id, "style": "expanded",
					"isMultiSelect": q.Multiple, "choices": choices, "isRequired": true,
					"errorMessage": "Please choose an answer",
				})
			} else {
				body = append(body, map[string]any{
					"type": "Input.Text", "id": id, "isMultiline": true, "maxLength": 2000,
					"isRequired": true, "errorMessage": "Please answer this question",
				})
			}
		}
		actions = []any{submit("Send", "positive", p.Nonce, p.OwnerID, "answer")}
	}
	if who != "" {
		body = append(body, textBlock(who, subtle))
	}
	return newCard(body, actions)
}

func tooLargeCard(p bridge.Prompt) card {
	switch p.Kind {
	case store.PendingToolApproval:
		return newCard([]any{
			textBlock("Approval needed", heading),
			textBlock(fmt.Sprintf("The agent wants to run %d tool call(s), but the request is too large to show in Teams. "+
				"It can't be approved here: reject it, or review it in the agent's own interface.", len(p.Tools))),
		}, []any{submit("Reject", "destructive", p.Nonce, p.OwnerID, "reject")})
	default:
		return newCard([]any{
			textBlock("The agent has a question", heading),
			textBlock("The question is too large to show in Teams. Please answer it in the agent's own interface."),
		}, nil)
	}
}

// SummaryCard replaces an answered prompt so it cannot be submitted twice.
func SummaryCard(summary string) card {
	return newCard([]any{textBlock(summary)}, nil)
}
