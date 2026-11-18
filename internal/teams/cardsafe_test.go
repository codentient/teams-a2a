package teams

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codentient/teams-a2a/internal/testagent"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCardsAreDroppedWhenDisabled(t *testing.T) {
	if _, ok := SanitizeCard(testagent.MaliciousCard, CardPolicy{}); ok {
		t.Fatal("agent cards must be dropped unless the route enables them")
	}
}

func TestSanitizeCardRemovesExfiltrationPhishingAndForgedActions(t *testing.T) {
	out, ok := SanitizeCard(testagent.MaliciousCard, CardPolicy{Enabled: true, AllowedHosts: []string{"*.example.com"}})
	if !ok {
		t.Fatal("a card with safe content left must be kept")
	}
	got := mustJSON(t, out)
	for _, bad := range []string{
		"attacker.example", "SECRET", "micros0ft", // exfiltration and phishing hosts
		"Action.Submit", "Action.Execute", "imBack", "msteams", // forged or invoke actions
		"selectAction", "backgroundImage", "refresh", "Input.Text", // hidden channels and inputs
	} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitised card still contains %q: %s", bad, got)
		}
	}
	// The ShowCard held only an attacker image and an Execute action, so nothing of it is left.
	if strings.Contains(got, "Action.ShowCard") {
		t.Errorf("a ShowCard with nothing safe inside must be dropped: %s", got)
	}
	for _, good := range []string{"Approval needed", "Nested text", "https://docs.example.com/diagram.png", "https://docs.example.com/runbook"} {
		if !strings.Contains(got, good) {
			t.Errorf("sanitised card lost safe content %q: %s", good, got)
		}
	}
}

func TestSanitizeCardRules(t *testing.T) {
	p := CardPolicy{Enabled: true, AllowedHosts: []string{"docs.example.com"}}
	card := func(body ...any) map[string]any { return map[string]any{"type": "AdaptiveCard", "body": body} }
	img := func(url string) map[string]any { return map[string]any{"type": "Image", "url": url} }
	cases := map[string]struct {
		in   any
		keep bool
		want string // substring that must survive, if kept
	}{
		"data png image kept":       {card(img("data:image/png;base64,iVBORw0KGgo=")), true, "data:image/png"},
		"data svg image dropped":    {card(img("data:image/svg+xml;base64,PHN2Zz4=")), false, ""},
		"http image dropped":        {card(img("http://docs.example.com/a.png")), false, ""},
		"subdomain not allowlisted": {card(img("https://evil.docs.example.com.attacker.test/a.png")), false, ""},
		"exact host allowlisted":    {card(img("https://docs.example.com/a.png")), true, "docs.example.com/a.png"},
		"userinfo in url dropped":   {card(img("https://docs.example.com@attacker.test/a.png")), false, ""},
		"not an adaptive card":      {map[string]any{"type": "HeroCard", "body": []any{}}, false, ""},
		"not an object":             {"<b>html</b>", false, ""},
		"empty after sanitising":    {card(map[string]any{"type": "Input.Text", "id": "x"}), false, ""},
		"fact set kept":             {card(map[string]any{"type": "FactSet", "facts": []any{map[string]any{"title": "Pod", "value": "web-1"}}}), true, "web-1"},
		"table kept":                {card(map[string]any{"type": "Table", "columns": []any{map[string]any{"width": 1}}, "rows": []any{map[string]any{"type": "TableRow", "cells": []any{map[string]any{"type": "TableCell", "items": []any{map[string]any{"type": "TextBlock", "text": "cell"}}}}}}}), true, "cell"},
		"show card sanitised recursively": {map[string]any{"type": "AdaptiveCard", "actions": []any{map[string]any{"type": "Action.ShowCard", "title": "More",
			"card": map[string]any{"type": "AdaptiveCard", "body": []any{map[string]any{"type": "TextBlock", "text": "details"}},
				"actions": []any{map[string]any{"type": "Action.Submit", "data": "approve"}}}}}}, true, "details"},
		"rich text run select drop": {card(map[string]any{"type": "RichTextBlock", "inlines": []any{map[string]any{"type": "TextRun", "text": "run", "selectAction": map[string]any{"type": "Action.Submit"}}}}), true, "run"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			out, ok := SanitizeCard(tc.in, p)
			if ok != tc.keep {
				t.Fatalf("kept = %v, want %v (%s)", ok, tc.keep, mustJSON(t, out))
			}
			if ok {
				got := mustJSON(t, out)
				if !strings.Contains(got, tc.want) {
					t.Errorf("want %q in %s", tc.want, got)
				}
				if strings.Contains(got, "selectAction") || strings.Contains(got, "Action.Submit") {
					t.Errorf("unsafe action survived: %s", got)
				}
			}
		})
	}
}

func TestSanitizeCardLimits(t *testing.T) {
	p := CardPolicy{Enabled: true}
	// Deep nesting.
	var inner any = map[string]any{"type": "TextBlock", "text": "deep"}
	for range 40 {
		inner = map[string]any{"type": "Container", "items": []any{inner}}
	}
	if _, ok := SanitizeCard(map[string]any{"type": "AdaptiveCard", "body": []any{inner}}, p); ok {
		t.Error("a card nested beyond the depth limit must be dropped")
	}
	// Too many elements.
	var many []any
	for range 1000 {
		many = append(many, map[string]any{"type": "TextBlock", "text": "x"})
	}
	if _, ok := SanitizeCard(map[string]any{"type": "AdaptiveCard", "body": many}, p); ok {
		t.Error("a card with too many elements must be dropped")
	}
	// Too large.
	big := strings.Repeat("x", 40_000)
	if _, ok := SanitizeCard(map[string]any{"type": "AdaptiveCard", "body": []any{map[string]any{"type": "TextBlock", "text": big}}}, p); ok {
		t.Error("a card above the size limit must be dropped")
	}
}
