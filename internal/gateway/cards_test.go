package gateway_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/codentient/teams-a2a/internal/config"
	"github.com/codentient/teams-a2a/internal/testbot"
)

func attachments(c testbot.Call) (cards []string, images []string) {
	atts, _ := c.Body["attachments"].([]any)
	for _, a := range atts {
		m, _ := a.(map[string]any)
		switch ct, _ := m["contentType"].(string); {
		case ct == "application/vnd.microsoft.card.adaptive":
			b, _ := json.Marshal(m["content"])
			cards = append(cards, string(b))
		case strings.HasPrefix(ct, "image/"):
			images = append(images, ct+" "+toString(m["name"]))
		}
	}
	return cards, images
}

func toString(v any) string { s, _ := v.(string); return s }

func TestAgentCardsAreNotShownByDefault(t *testing.T) {
	e := newEnv(t, opts{})
	e.send(e.activity("malicious card"))
	final := e.waitFinal("Here is the report.")
	cards, images := attachments(final)
	t.Logf("Teams received text %q, cards %v, images %v", final.Text(), cards, images)
	if len(cards) != 0 {
		t.Fatalf("agent cards must not reach Teams unless enabled: %v", cards)
	}
	if len(images) != 1 || images[0] != "image/png graph.png" {
		t.Errorf("only the real PNG may be attached, got %v", images)
	}
	if !strings.Contains(final.Text(), "card") {
		t.Errorf("the user should be told a card was not shown: %q", final.Text())
	}
}

func TestAgentCardsAreSanitisedWhenEnabled(t *testing.T) {
	e := newEnv(t, opts{mutate: func(c *config.Config) {
		c.Routes[0].Agent.Cards = config.Cards{Enabled: true, AllowedHosts: []string{"docs.example.com"}}
	}})
	e.send(e.activity("malicious card"))
	final := e.waitFinal("Here is the report.")
	cards, images := attachments(final)
	t.Logf("Teams received text %q, images %v, card %s", final.Text(), images, cards)
	if len(cards) != 1 {
		t.Fatalf("want the sanitised card, got %v", cards)
	}
	for _, bad := range []string{"attacker.example", "SECRET", "micros0ft", "Action.Submit", "Action.Execute", "imBack", "selectAction", "backgroundImage", "refresh", "Input.Text"} {
		if strings.Contains(cards[0], bad) {
			t.Errorf("sanitised card still contains %q: %s", bad, cards[0])
		}
	}
	if !strings.Contains(cards[0], "https://docs.example.com/runbook") {
		t.Errorf("allowlisted link was removed: %s", cards[0])
	}
}
