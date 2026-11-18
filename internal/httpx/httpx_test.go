package httpx

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestNoRedirectsReturnsTheRedirectAndLeavesTheOriginalClientAlone(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits.Add(1) }))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	orig := &http.Client{}
	c := NoRedirects(orig)
	resp, err := c.Post(origin.URL, "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect || hits.Load() != 0 {
		t.Fatalf("redirect was followed: status %d, target hits %d", resp.StatusCode, hits.Load())
	}
	if orig.CheckRedirect != nil {
		t.Fatal("the caller's client must not be modified")
	}
	if NoRedirects(nil) == nil {
		t.Fatal("nil must yield a usable client")
	}
}
