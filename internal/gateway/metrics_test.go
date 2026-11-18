package gateway_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The success rate of /api/messages must be measurable (alerting, canary analysis), not only
// authentication failures; and the running version must be visible.
func TestRequestMetricsAndBuildInfo(t *testing.T) {
	e := newEnv(t, opts{})
	post := func(auth, body string) {
		req, _ := http.NewRequest(http.MethodPost, e.gw.URL+"/api/messages", strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post("", "{}")                // 401
	post("Bearer x", "{not json") // 400
	e.send(e.activity("hello"))   // 200
	e.waitFinal("Echo: hello")

	admin := httptest.NewServer(e.srv.AdminHandler())
	defer admin.Close()
	resp, err := http.Get(admin.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := string(b)
	for _, want := range []string{
		`teams_a2a_http_requests_total{code="200"} 1`,
		`teams_a2a_http_requests_total{code="400"} 1`,
		`teams_a2a_http_requests_total{code="401"} 1`,
		`teams_a2a_http_request_duration_seconds_count{code="200"} 1`,
		`teams_a2a_build_info{version=`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("/metrics lacks %s", want)
		}
	}
}
