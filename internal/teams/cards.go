package teams

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/codentient/teams-a2a/internal/bridge"
	"github.com/codentient/teams-a2a/internal/store"
)

const (
	maxToolsOnCard = 10
	maxArgsChars   = 1500
)

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

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// PromptCard renders a human-in-the-loop request.
func PromptCard(p bridge.Prompt, requesterOnly bool) card {
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
		intro += ":"
		if p.Hint != "" {
			intro = p.Hint
		}
		body = append(body, textBlock(intro))
		if p.Subagent != "" {
			body = append(body, textBlock("Requested by sub-agent **"+p.Subagent+"**", subtle))
		}
		for i, t := range p.Tools {
			if i == maxToolsOnCard {
				body = append(body, textBlock(fmt.Sprintf("…and %d more", len(p.Tools)-maxToolsOnCard), subtle))
				break
			}
			body = append(body, textBlock(t.Name, bold))
			if len(t.Args) > 0 {
				b, _ := json.MarshalIndent(t.Args, "", "  ")
				body = append(body, textBlock(truncateRunes(string(b), maxArgsChars), mono))
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

// SummaryCard replaces an answered prompt so it cannot be submitted twice.
func SummaryCard(summary string) card {
	return newCard([]any{textBlock(summary)}, nil)
}
