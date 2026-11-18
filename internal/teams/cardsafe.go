package teams

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
)

// CardPolicy decides whether and how agent-supplied Adaptive Cards reach Teams.
//
// Agent output is untrusted (prompt injection), and an Adaptive Card is an active document:
// Teams fetches its images when the message is shown, and its buttons make the clicking user
// send text, invoke the bot or open URLs. Cards are therefore dropped unless a route enables
// them, and enabled cards keep only display elements, images from data URLs or allowlisted
// hosts, and links to allowlisted hosts.
type CardPolicy struct {
	Enabled bool
	// AllowedHosts are hosts (exact or "*.suffix") whose https URLs may be used by Image
	// elements and Action.OpenUrl.
	AllowedHosts []string
}

const (
	maxCardDepth    = 12
	maxCardElements = 300
)

var errCardLimit = errors.New("card exceeds limits")

type propSet map[string]bool

func props(names ...string) propSet {
	s := propSet{}
	for _, n := range names {
		s[n] = true
	}
	return s
}

// Scalar layout properties every allowed element may keep.
var commonProps = props("type", "id", "isVisible", "spacing", "separator", "height", "horizontalAlignment", "targetWidth")

// Scalar properties per element type. Structural properties (items, columns, rows, cells,
// facts, inlines, images, actions, url) are handled explicitly.
var elementProps = map[string]propSet{
	"TextBlock":     props("text", "wrap", "size", "weight", "color", "isSubtle", "maxLines", "fontType", "style"),
	"RichTextBlock": props(),
	"FactSet":       props(),
	"Container":     props("style", "bleed", "minHeight", "verticalContentAlignment", "rtl"),
	"ColumnSet":     props("style", "bleed", "minHeight"),
	"Column":        props("width", "style", "bleed", "minHeight", "verticalContentAlignment", "rtl"),
	"Table":         props("firstRowAsHeader", "showGridLines", "gridStyle", "horizontalCellContentAlignment", "verticalCellContentAlignment"),
	"Image":         props("altText", "size", "style", "width"),
	"ImageSet":      props("imageSize"),
	"ActionSet":     props(),
}

var textRunProps = props("type", "text", "color", "fontType", "highlight", "isSubtle", "italic", "size", "strikethrough", "underline", "weight")

var safeImageMediaTypes = []string{"image/png", "image/jpeg", "image/gif", "image/webp"}

type sanitizer struct {
	hosts []string
	seen  int // elements visited, for the element limit
}

// SanitizeCard returns a safe copy of an agent-supplied Adaptive Card, or false when the card
// must not be shown: cards are disabled, the input is not an Adaptive Card, it exceeds the size,
// depth or element limits, or nothing displayable is left.
func SanitizeCard(card any, p CardPolicy) (map[string]any, bool) {
	if !p.Enabled {
		return nil, false
	}
	s := &sanitizer{hosts: p.AllowedHosts}
	out, err := s.card(card, 0)
	if err != nil || out == nil {
		return nil, false
	}
	b, err := json.Marshal(out)
	if err != nil || len(b) > maxCardBytes {
		return nil, false
	}
	return out, true
}

func (s *sanitizer) card(v any, depth int) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok || m["type"] != "AdaptiveCard" {
		return nil, nil
	}
	out := map[string]any{"type": "AdaptiveCard"}
	for _, k := range []string{"version", "lang", "minHeight", "verticalContentAlignment", "rtl"} {
		if sv, ok := scalar(m[k]); ok {
			out[k] = sv
		}
	}
	body, err := s.elements(m["body"], depth+1)
	if err != nil {
		return nil, err
	}
	actions, err := s.actions(m["actions"], depth+1)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 && len(actions) == 0 {
		return nil, nil
	}
	if len(body) > 0 {
		out["body"] = body
	}
	if len(actions) > 0 {
		out["actions"] = actions
	}
	return out, nil
}

func (s *sanitizer) elements(v any, depth int) ([]any, error) {
	list, _ := v.([]any)
	var out []any
	for _, item := range list {
		el, err := s.element(item, depth)
		if err != nil {
			return nil, err
		}
		if el != nil {
			out = append(out, el)
		}
	}
	return out, nil
}

func (s *sanitizer) count(depth int) error {
	s.seen++
	if depth > maxCardDepth || s.seen > maxCardElements {
		return errCardLimit
	}
	return nil
}

func (s *sanitizer) element(v any, depth int) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, nil
	}
	typ, _ := m["type"].(string)
	allowed, known := elementProps[typ]
	if !known {
		return nil, nil // inputs, media and unknown elements are dropped
	}
	if err := s.count(depth); err != nil {
		return nil, err
	}
	out := copyScalars(m, commonProps, allowed)
	var err error
	switch typ {
	case "TextBlock":
		if _, ok := out["text"]; !ok {
			return nil, nil
		}
	case "RichTextBlock":
		inlines := s.inlines(m["inlines"])
		if len(inlines) == 0 {
			return nil, nil
		}
		out["inlines"] = inlines
	case "FactSet":
		facts := facts(m["facts"])
		if len(facts) == 0 {
			return nil, nil
		}
		out["facts"] = facts
	case "Container", "Column":
		items, err := s.elements(m["items"], depth+1)
		if err != nil {
			return nil, err
		}
		if len(items) == 0 {
			return nil, nil
		}
		out["items"] = items
	case "ColumnSet":
		columns, err := s.columns(m["columns"], depth+1)
		if err != nil {
			return nil, err
		}
		if len(columns) == 0 {
			return nil, nil
		}
		out["columns"] = columns
	case "Table":
		out["columns"] = tableColumns(m["columns"])
		if out["rows"], err = s.rows(m["rows"], depth+1); err != nil {
			return nil, err
		}
	case "Image":
		u, ok := s.url(m["url"], true)
		if !ok {
			return nil, nil
		}
		out["url"] = u
	case "ImageSet":
		var images []any
		for _, img := range asList(m["images"]) {
			el, err := s.element(img, depth+1)
			if err != nil {
				return nil, err
			}
			if el != nil && el["type"] == "Image" {
				images = append(images, el)
			}
		}
		if len(images) == 0 {
			return nil, nil
		}
		out["images"] = images
	case "ActionSet":
		actions, err := s.actions(m["actions"], depth+1)
		if err != nil {
			return nil, err
		}
		if len(actions) == 0 {
			return nil, nil
		}
		out["actions"] = actions
	}
	return out, nil
}

func (s *sanitizer) columns(v any, depth int) ([]any, error) {
	var out []any
	for _, c := range asList(v) {
		if cm, ok := c.(map[string]any); ok {
			if _, has := cm["type"]; !has {
				cm = withType(cm, "Column")
			}
			if cm["type"] != "Column" {
				continue
			}
			el, err := s.element(cm, depth)
			if err != nil {
				return nil, err
			}
			if el != nil {
				out = append(out, el)
			}
		}
	}
	return out, nil
}

func (s *sanitizer) rows(v any, depth int) ([]any, error) {
	var rows []any
	for _, r := range asList(v) {
		rm, ok := r.(map[string]any)
		if !ok {
			continue
		}
		if err := s.count(depth); err != nil {
			return nil, err
		}
		row := map[string]any{"type": "TableRow"}
		if st, ok := scalar(rm["style"]); ok {
			row["style"] = st
		}
		var cells []any
		for _, c := range asList(rm["cells"]) {
			cm, ok := c.(map[string]any)
			if !ok {
				continue
			}
			if err := s.count(depth + 1); err != nil {
				return nil, err
			}
			items, err := s.elements(cm["items"], depth+2)
			if err != nil {
				return nil, err
			}
			cell := map[string]any{"type": "TableCell", "items": items}
			if st, ok := scalar(cm["style"]); ok {
				cell["style"] = st
			}
			cells = append(cells, cell)
		}
		row["cells"] = cells
		rows = append(rows, row)
	}
	return rows, nil
}

func tableColumns(v any) []any {
	var out []any
	for _, c := range asList(v) {
		if cm, ok := c.(map[string]any); ok {
			out = append(out, copyScalars(cm, props("width", "horizontalCellContentAlignment", "verticalCellContentAlignment")))
		}
	}
	return out
}

func (s *sanitizer) inlines(v any) []any {
	var out []any
	for _, in := range asList(v) {
		switch x := in.(type) {
		case string:
			out = append(out, x)
		case map[string]any:
			if x["type"] == "TextRun" {
				if run := copyScalars(x, textRunProps); run["text"] != nil {
					out = append(out, run)
				}
			}
		}
	}
	return out
}

func facts(v any) []any {
	var out []any
	for _, f := range asList(v) {
		if fm, ok := f.(map[string]any); ok {
			title, tok := fm["title"].(string)
			value, vok := fm["value"].(string)
			if tok || vok {
				out = append(out, map[string]any{"title": title, "value": value})
			}
		}
	}
	return out
}

// actions keeps Action.OpenUrl to allowlisted https hosts, Action.ShowCard with a sanitised
// card, and Action.ToggleVisibility. Submit, Execute and everything that makes the user send
// data or invoke the bot is dropped.
func (s *sanitizer) actions(v any, depth int) ([]any, error) {
	var out []any
	for _, a := range asList(v) {
		am, ok := a.(map[string]any)
		if !ok {
			continue
		}
		title, _ := am["title"].(string)
		switch am["type"] {
		case "Action.OpenUrl":
			u, ok := s.url(am["url"], false)
			if !ok {
				continue
			}
			out = append(out, map[string]any{"type": "Action.OpenUrl", "title": title, "url": u})
		case "Action.ShowCard":
			if err := s.count(depth); err != nil {
				return nil, err
			}
			card, err := s.card(am["card"], depth+1)
			if err != nil {
				return nil, err
			}
			if card != nil {
				out = append(out, map[string]any{"type": "Action.ShowCard", "title": title, "card": card})
			}
		case "Action.ToggleVisibility":
			var targets []any
			for _, t := range asList(am["targetElements"]) {
				switch x := t.(type) {
				case string:
					targets = append(targets, x)
				case map[string]any:
					targets = append(targets, copyScalars(x, props("elementId", "isVisible")))
				}
			}
			if len(targets) > 0 {
				out = append(out, map[string]any{"type": "Action.ToggleVisibility", "title": title, "targetElements": targets})
			}
		}
	}
	return out, nil
}

// url accepts https URLs on allowlisted hosts and, for images, base64 data URLs of raster types.
func (s *sanitizer) url(v any, image bool) (string, bool) {
	raw, ok := v.(string)
	if !ok {
		return "", false
	}
	if image {
		for _, mt := range safeImageMediaTypes {
			if data, ok := strings.CutPrefix(raw, "data:"+mt+";base64,"); ok {
				if _, err := base64.StdEncoding.DecodeString(data); err == nil {
					return raw, true
				}
				return "", false
			}
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Host == "" || !hostAllowed(u.Hostname(), s.hosts) {
		return "", false
	}
	return u.String(), true
}

func copyScalars(m map[string]any, allowed ...propSet) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		for _, set := range allowed {
			if set[k] {
				if sv, ok := scalar(v); ok {
					out[k] = sv
				}
				break
			}
		}
	}
	return out
}

func scalar(v any) (any, bool) {
	switch v.(type) {
	case string, bool, float64, int, int64:
		return v, true
	}
	return nil, false
}

func asList(v any) []any { l, _ := v.([]any); return l }

func withType(m map[string]any, typ string) map[string]any {
	out := make(map[string]any, len(m)+1)
	for k, v := range m {
		out[k] = v
	}
	out["type"] = typ
	return out
}
