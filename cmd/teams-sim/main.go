// Command teams-sim plays the Microsoft side of a Teams bot so the gateway binary can be
// exercised locally without Azure: it serves Bot Connector OpenID metadata and keys, an Entra
// token endpoint and the Connector API, sends signed activities to the gateway and prints
// whatever the gateway posts back.
//
//	go run ./cmd/teams-sim -listen 127.0.0.1:4000 -gateway http://127.0.0.1:3978 "hello" "delete pod web-1" approve
//
// Configure the gateway with teams.openIdMetadataURL=http://127.0.0.1:4000/v1/.well-known/openidconfiguration,
// teams.loginAuthority=http://127.0.0.1:4000, serviceUrlAllowlist [127.0.0.1] and allowInsecureServiceURLs.
// The word "approve" / "reject" presses the corresponding button on the last approval card.
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codentient/teams-a2a/internal/testbot"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:4000", "address for the fake Microsoft endpoints")
	gateway := flag.String("gateway", "http://127.0.0.1:3978", "gateway base URL")
	appID := flag.String("app-id", "00000000-0000-0000-0000-000000000001", "bot app id")
	clientSecret := flag.String("client-secret", "", "if set, the fake token endpoint only accepts this secret for the bot app id")
	tenant := flag.String("tenant", "00000000-0000-0000-0000-0000000000aa", "tenant id")
	wait := flag.Duration("wait", 20*time.Second, "how long to wait for each reply")
	publicURL := flag.String("public-url", "", "URL the gateway uses to reach this simulator (default: the listen address)")
	readyURL := flag.String("ready-url", "http://127.0.0.1:9090/readyz", "gateway readiness URL to wait for before sending")
	flag.Parse()

	l, err := net.Listen("tcp", *listen)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fake := testbot.NewOn(l)
	if *clientSecret != "" {
		fake.SetClientSecret(*appID, *clientSecret)
	}
	if *publicURL != "" {
		fake.Server.URL = strings.TrimRight(*publicURL, "/")
	}
	fmt.Printf("fake Microsoft endpoints on %s; waiting for %s\n", fake.Server.URL, *readyURL)
	deadline := time.Now().Add(60 * time.Second)
	for {
		resp, err := http.Get(*readyURL)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			fmt.Fprintln(os.Stderr, "gateway did not become ready")
			os.Exit(1)
		}
		time.Sleep(200 * time.Millisecond)
	}

	serviceURL := fake.Server.URL + "/"
	var lastCard map[string]any
	for i, text := range flag.Args() {
		fake.Reset()
		act := map[string]any{
			"type": "message", "id": fmt.Sprintf("sim-%d-%d", time.Now().Unix(), i), "channelId": "msteams",
			"serviceUrl":   serviceURL,
			"from":         map[string]any{"id": "29:sim-user", "aadObjectId": "00000000-0000-0000-0000-00000000a11c", "name": "Sim User"},
			"recipient":    map[string]any{"id": "28:" + *appID},
			"conversation": map[string]any{"id": "a:sim-personal", "conversationType": "personal", "tenantId": *tenant},
			"channelData":  map[string]any{"tenant": map[string]any{"id": *tenant}},
		}
		if (text == "approve" || text == "reject") && lastCard != nil {
			act["value"] = findSubmit(lastCard, strings.ToUpper(text[:1])+text[1:])
		} else {
			act["text"] = text
		}
		fmt.Printf("\n> %s\n", text)
		b, _ := json.Marshal(act)
		status, err := post(*gateway+"/api/messages", "Bearer "+fake.Token(*appID, serviceURL, nil), b)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("  gateway answered HTTP %d\n", status)

		// A turn is over when a final message or a prompt card arrives.
		_, ok := fake.WaitFor(*wait, func(c testbot.Call) bool {
			return (c.Type() == "message" && c.Method == http.MethodPost && (c.StreamInfo() == nil || c.StreamInfo()["streamType"] == "final") && c.Text() != "_Waiting for your input…_") || (c.Card() != nil && c.Method == http.MethodPost)
		})
		time.Sleep(300 * time.Millisecond)
		for _, c := range fake.Calls() {
			switch {
			case c.Card() != nil && c.Method == http.MethodPost:
				lastCard = c.Card()
				j, _ := json.Marshal(c.Card()["body"])
				fmt.Printf("  [card] %s\n", j)
			case c.Method == http.MethodPut:
				fmt.Printf("  [card updated] %v\n", c.Card()["body"])
			case c.Type() == "typing" && c.StreamInfo() != nil:
				fmt.Printf("  [stream %v #%v] %q\n", c.StreamInfo()["streamType"], c.StreamInfo()["streamSequence"], c.Text())
			case c.Type() == "message":
				fmt.Printf("  [message] %q\n", c.Text())
			}
		}
		if !ok {
			fmt.Println("  (timed out waiting for a reply)")
			os.Exit(2)
		}
	}
}

// post sends an activity, retrying connection errors for a while: in Kubernetes a Service can
// lag behind the pod's readiness. Any HTTP response, including an error status, is final; the
// gateway de-duplicates activity ids, so a retry can never run a turn twice.
func post(url, authz string, body []byte) (int, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, _ := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
		req.Header.Set("Authorization", authz)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
			return resp.StatusCode, nil
		}
		if time.Now().After(deadline) {
			return 0, err
		}
		fmt.Printf("  (gateway not reachable yet, retrying: %v)\n", err)
		time.Sleep(time.Second)
	}
}

func findSubmit(card map[string]any, title string) map[string]any {
	actions, _ := card["actions"].([]any)
	for _, a := range actions {
		if m, ok := a.(map[string]any); ok && m["title"] == title {
			d, _ := m["data"].(map[string]any)
			return d
		}
	}
	return nil
}
