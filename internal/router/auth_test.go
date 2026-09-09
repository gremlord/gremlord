package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// authProbe posts to the reload endpoint, which sits behind auth but does not
// touch an upstream, with whichever credential headers the case supplies.
func authProbe(t *testing.T, ts *httptest.Server, apiKey, bearer string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/messages/count_tokens", strings.NewReader(`{}`))
	req.Header.Set("content-type", "application/json")
	if apiKey != "" {
		req.Header.Set("x-api-key", apiKey)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// TestAuthAcceptsLocalTokenFromEitherHeader covers the shape Claude Code sends
// when a /login managed key and ANTHROPIC_AUTH_TOKEN are both present: the
// managed key rides in x-api-key and the local token in Authorization. The
// router must recognise its own token wherever it appears, and still refuse
// requests that carry it in neither header.
func TestAuthAcceptsLocalTokenFromEitherHeader(t *testing.T) {
	ts, _ := newTestServer(t, "http://127.0.0.1:0", nil)

	cases := []struct {
		name     string
		apiKey   string
		bearer   string
		wantAuth bool
	}{
		{"x-api-key only", testToken, "", true},
		{"bearer only", "", testToken, true},
		{"managed key in x-api-key, local token in bearer", "sk-ant-managed-key", testToken, true},
		{"local token in x-api-key, foreign bearer", testToken, "some-oauth-token", true},
		{"foreign key in both", "sk-ant-managed-key", "some-oauth-token", false},
		{"no credentials", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := authProbe(t, ts, tc.apiKey, tc.bearer)
			if tc.wantAuth && got == http.StatusUnauthorized {
				t.Fatalf("expected the local token to be accepted, got 401")
			}
			if !tc.wantAuth && got != http.StatusUnauthorized {
				t.Fatalf("expected 401, got %d", got)
			}
		})
	}
}
