// Command demo-agent serves a kagent-like A2A v1.0 agent for local development:
//
//	go run ./cmd/demo-agent -listen :8083
//
// Its card lives at /agents/kagent/demo/.well-known/agent-card.json. It echoes messages as a
// stream; "delete ..." asks for tool approval, "scale ..." asks a question, "fail" fails.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/codentient/teams-a2a/internal/testagent"
)

func main() {
	listen := flag.String("listen", "127.0.0.1:8083", "listen address")
	base := flag.String("base-url", "http://127.0.0.1:8083", "origin advertised in the agent card")
	delay := flag.Duration("chunk-delay", 300*time.Millisecond, "delay between streamed chunks")
	flag.Parse()

	a := &testagent.Agent{ChunkDelay: *delay}
	srv := &http.Server{Addr: *listen, Handler: a.Handler(*base, "/agents/kagent/demo"), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("demo agent on %s, card at %s/agents/kagent/demo/.well-known/agent-card.json", *listen, *base)
	log.Fatal(srv.ListenAndServe())
}
