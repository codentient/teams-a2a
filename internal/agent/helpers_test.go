package agent

import "github.com/a2aproject/a2a-go/v2/a2a"

func cardWith(urls ...string) *a2a.AgentCard {
	c := &a2a.AgentCard{Name: "x"}
	for _, u := range urls {
		c.SupportedInterfaces = append(c.SupportedInterfaces, a2a.NewAgentInterface(u, a2a.TransportProtocolJSONRPC))
	}
	return c
}
