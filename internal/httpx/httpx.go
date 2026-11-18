// Package httpx holds HTTP client helpers shared by the gateway's outbound callers.
package httpx

import "net/http"

// NoRedirects returns a copy of hc that never follows redirects: the 3xx response is returned
// to the caller, which treats it as an error.
//
// Every outbound call of the gateway carries a credential, user data or a trust decision
// (agent bearer tokens and identity headers, OAuth2 client secrets, the bot token, Bot
// Connector signing keys). Following a 307/308 would replay them to a host that origin
// pinning and allowlists never approved; net/http keeps Authorization on a redirect to the
// same host with another port, and always resends the body. The caller's client is unchanged.
func NoRedirects(hc *http.Client) *http.Client {
	if hc == nil {
		hc = http.DefaultClient
	}
	c := *hc
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c
}
