package teams

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestServiceURLPolicy(t *testing.T) {
	p := ServiceURLPolicy{Allowlist: []string{"smba.trafficmanager.net", "*.botframework.com"}}
	ok := []string{
		"https://smba.trafficmanager.net/emea/",
		"https://smba.trafficmanager.net/amer/tenant/",
		"https://europe.webchat.botframework.com/",
	}
	bad := []string{
		"http://smba.trafficmanager.net/emea/",         // plain http would leak the token
		"https://evil.trafficmanager.net/",             // anyone can register *.trafficmanager.net
		"https://botframework.com.evil.example/",       // suffix trick
		"https://evilbotframework.com/",                // missing dot boundary
		"https://user:pw@smba.trafficmanager.net/",     // embedded credentials
		"https://smba.trafficmanager.net/emea/?x=1",    // query string
		"ftp://smba.trafficmanager.net/",               // scheme
		"https://smba.trafficmanager.net.evil.example", // host suffix
		"not a url",
	}
	for _, u := range ok {
		if _, err := p.Check(u); err != nil {
			t.Errorf("%s should be allowed: %v", u, err)
		}
	}
	for _, u := range bad {
		if _, err := p.Check(u); err == nil {
			t.Errorf("%s should be rejected", u)
		}
	}
}

func TestSplitText(t *testing.T) {
	para := strings.Repeat("a", 60)
	text := strings.Join([]string{para, para, para, para}, "\n\n")
	chunks := splitText(text, 130)
	if len(chunks) != 2 {
		t.Fatalf("got %d chunks, want 2: %q", len(chunks), chunks)
	}
	if strings.Join(chunks, "\n\n") != text {
		t.Errorf("splitting at paragraph boundaries must not lose content")
	}
	multibyte := strings.Repeat("é", 250)
	for _, c := range splitText(multibyte, 100) {
		if !utf8.ValidString(c) || utf8.RuneCountInString(c) > 100 {
			t.Errorf("bad chunk %q", c)
		}
	}
	if got := splitText("", 10); len(got) != 1 || got[0] != "" {
		t.Errorf("empty text: %q", got)
	}
}

func TestTextWithoutBotMention(t *testing.T) {
	a := &Activity{
		Text:      "<at>Ops Bot</at> restart <at>Bob</at>'s pod",
		Recipient: &ChannelAccount{ID: "28:bot"},
		Entities: []jsonRaw{
			jsonRaw(`{"type":"mention","text":"<at>Ops Bot</at>","mentioned":{"id":"28:bot"}}`),
			jsonRaw(`{"type":"mention","text":"<at>Bob</at>","mentioned":{"id":"29:bob"}}`),
		},
	}
	if got := a.TextWithoutBotMention(); got != "restart <at>Bob</at>'s pod" {
		t.Errorf("got %q", got)
	}
}

type jsonRaw = json.RawMessage
