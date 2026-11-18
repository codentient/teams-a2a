package bridge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

const (
	partTypeKey   = "kagent.dev/a2a/part-type"
	thoughtKey    = "kagent.dev/a2a/thought"
	adkTypeKey    = "adk_type" // older ADK-based runtimes
	adaptiveCard  = "application/vnd.microsoft.card.adaptive"
	maxDataRender = 4000
	maxImageBytes = 1 << 20
)

// Image is an inline image produced by the agent.
type Image struct {
	MediaType string
	Name      string
	DataURL   string
}

// Reply is a channel-neutral final answer.
type Reply struct {
	Text    string
	Cards   []any
	Images  []Image
	IsError bool
}

type artifactBuf struct {
	id    a2a.ArtifactID
	parts []*a2a.Part
}

// accumulator folds a stream of A2A events into the latest view of a turn.
type accumulator struct {
	taskID    string
	contextID string
	state     a2a.TaskState
	status    *a2a.Message
	direct    *a2a.Message
	order     []a2a.ArtifactID
	artifacts map[a2a.ArtifactID]*artifactBuf
}

func newAccumulator() *accumulator {
	return &accumulator{artifacts: map[a2a.ArtifactID]*artifactBuf{}}
}

// update describes what changed after applying one event.
type update struct {
	textChanged bool
	progress    []string
}

func toolActivity(p *a2a.Part) (kind, name string, ok bool) {
	kind, _ = p.Metadata[partTypeKey].(string)
	if kind == "" {
		kind, _ = p.Metadata[adkTypeKey].(string)
	}
	if kind != "function_call" && kind != "function_response" {
		return "", "", false
	}
	if m, isMap := p.Data().(map[string]any); isMap {
		name, _ = m["name"].(string)
	}
	return kind, name, true
}

func isThought(p *a2a.Part) bool {
	v, _ := p.Metadata[thoughtKey].(bool)
	return v
}

// progressFrom turns tool activity and status chatter into short informative lines.
func progressFrom(parts []*a2a.Part) []string {
	var out []string
	for _, p := range parts {
		if kind, name, ok := toolActivity(p); ok {
			if name == "" {
				name = "a tool"
			} else {
				name = "`" + name + "`"
			}
			if kind == "function_call" {
				out = append(out, "Running "+name+"…")
			} else {
				out = append(out, "Finished "+name+".")
			}
			continue
		}
		if isThought(p) {
			out = append(out, "Thinking…")
			continue
		}
		if t := strings.TrimSpace(p.Text()); t != "" {
			out = append(out, truncate(t, 200))
		}
	}
	return out
}

func (a *accumulator) apply(ev a2a.Event) update {
	var u update
	before := a.text()
	switch e := ev.(type) {
	case *a2a.Message:
		a.direct = e
		if e.ContextID != "" {
			a.contextID = e.ContextID
		}
		a.state = a2a.TaskStateCompleted
	case *a2a.Task:
		a.applyTask(e)
	case *a2a.TaskStatusUpdateEvent:
		a.taskID, a.contextID = string(e.TaskID), e.ContextID
		a.state = e.Status.State
		a.status = e.Status.Message
		if e.Status.State == a2a.TaskStateWorking && e.Status.Message != nil {
			u.progress = progressFrom(e.Status.Message.Parts)
		}
	case *a2a.TaskArtifactUpdateEvent:
		a.taskID, a.contextID = string(e.TaskID), e.ContextID
		if e.Artifact != nil {
			for _, p := range e.Artifact.Parts {
				if _, _, ok := toolActivity(p); ok || isThought(p) {
					u.progress = append(u.progress, progressFrom([]*a2a.Part{p})...)
				}
			}
			a.applyArtifact(e.Artifact, e.Append)
		}
	}
	u.textChanged = a.text() != before
	return u
}

func (a *accumulator) applyTask(t *a2a.Task) {
	a.taskID, a.contextID = string(t.ID), t.ContextID
	a.state = t.Status.State
	a.status = t.Status.Message
	if len(t.Artifacts) > 0 {
		a.order, a.artifacts = nil, map[a2a.ArtifactID]*artifactBuf{}
		for _, art := range t.Artifacts {
			a.applyArtifact(art, false)
		}
	}
}

func (a *accumulator) applyArtifact(art *a2a.Artifact, appendParts bool) {
	buf, ok := a.artifacts[art.ID]
	if !ok {
		buf = &artifactBuf{id: art.ID}
		a.artifacts[art.ID] = buf
		a.order = append(a.order, art.ID)
	}
	if !appendParts {
		buf.parts = nil
	}
	for _, p := range art.Parts {
		// Streaming text chunks are merged into the previous text part.
		if p.Text() != "" && !isThought(p) && len(buf.parts) > 0 {
			last := buf.parts[len(buf.parts)-1]
			if _, isText := last.Content.(a2a.Text); isText && !isThought(last) {
				merged := *last
				merged.Content = a2a.Text(last.Text() + p.Text())
				buf.parts[len(buf.parts)-1] = &merged
				continue
			}
		}
		buf.parts = append(buf.parts, p)
	}
}

// outputParts are the parts that make up the user-visible answer.
func (a *accumulator) outputParts() []*a2a.Part {
	var parts []*a2a.Part
	for _, id := range a.order {
		parts = append(parts, a.artifacts[id].parts...)
	}
	if len(parts) == 0 && a.direct != nil {
		parts = a.direct.Parts
	}
	if len(parts) == 0 && a.state == a2a.TaskStateCompleted && a.status != nil {
		parts = a.status.Parts
	}
	return parts
}

func visible(p *a2a.Part) bool {
	if _, _, ok := toolActivity(p); ok {
		return false
	}
	return !isThought(p)
}

func (a *accumulator) text() string {
	var b strings.Builder
	for _, p := range a.outputParts() {
		if !visible(p) {
			continue
		}
		if t := p.Text(); t != "" {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString(t)
		}
	}
	return b.String()
}

// statusText is the human-readable text of the latest status message.
func (a *accumulator) statusText() string {
	if a.status == nil {
		return ""
	}
	var lines []string
	for _, p := range a.status.Parts {
		if visible(p) && strings.TrimSpace(p.Text()) != "" {
			lines = append(lines, p.Text())
		}
	}
	return strings.Join(lines, "\n")
}

// reply renders the complete answer, including non-text parts.
func (a *accumulator) reply() Reply {
	var r Reply
	var text []string
	for _, p := range a.outputParts() {
		if !visible(p) {
			continue
		}
		switch c := p.Content.(type) {
		case a2a.Text:
			if s := string(c); strings.TrimSpace(s) != "" {
				text = append(text, s)
			}
		case a2a.Data:
			if p.MediaType == adaptiveCard {
				r.Cards = append(r.Cards, c.Value)
				continue
			}
			b, err := json.MarshalIndent(c.Value, "", "  ")
			if err == nil {
				text = append(text, "```json\n"+truncate(string(b), maxDataRender)+"\n```")
			}
		case a2a.URL:
			if u, err := url.Parse(string(c)); err == nil && u.Scheme == "https" {
				label := p.Filename
				if label == "" {
					label = string(c)
				}
				text = append(text, fmt.Sprintf("[%s](%s)", escapeLinkText(label), u.String()))
			}
		case a2a.Raw:
			if mt, ok := safeImage(p.MediaType, c); ok {
				r.Images = append(r.Images, Image{
					MediaType: mt,
					Name:      p.Filename,
					DataURL:   "data:" + mt + ";base64," + base64.StdEncoding.EncodeToString(c),
				})
			} else {
				name := p.Filename
				if name == "" {
					name = "file"
				}
				text = append(text, fmt.Sprintf("_(The agent returned a file, %s, that cannot be shown here.)_", escapeLinkText(name)))
			}
		}
	}
	r.Text = strings.Join(text, "\n\n")
	return r
}

// safeImage accepts PNG, JPEG, GIF and WebP whose bytes match the declared type. Other image
// types (SVG can carry script and external references) and mislabelled data are not shown.
func safeImage(mediaType string, b []byte) (string, bool) {
	mt := strings.ToLower(strings.TrimSpace(strings.SplitN(mediaType, ";", 2)[0]))
	switch mt {
	case "image/png", "image/jpeg", "image/gif", "image/webp":
	default:
		return "", false
	}
	if len(b) == 0 || len(b) > maxImageBytes || http.DetectContentType(b) != mt {
		return "", false
	}
	return mt, true
}

func escapeLinkText(s string) string {
	return strings.NewReplacer("[", "(", "]", ")", "\n", " ").Replace(s)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8Boundary(s, cut) {
		cut--
	}
	return s[:cut] + "…"
}

func utf8Boundary(s string, i int) bool { return i >= len(s) || s[i]&0xC0 != 0x80 }
