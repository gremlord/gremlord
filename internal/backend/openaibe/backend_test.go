package openaibe

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
)

const minimalReq = `{"model":"gpt","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`

func callRoute(baseURL string) config.Resolved {
	return config.Resolved{
		Alias:        "gpt",
		ProviderName: "openai",
		Provider:     config.Provider{Type: "openai", BaseURL: baseURL},
		Model:        config.Model{ID: "gpt-5.2"},
	}
}

// TestMessagesUpstreamConnFailurePopulatesErrMsg covers the "transport
// connect error" translation-failure path: a base URL nothing listens on, so
// http.Client.Do itself fails. Before this fix Result.ErrMsg was left empty
// on this path, so server.go's `res.ErrMsg != ""` gate silently dropped the
// "upstream error" log line — the router-internal 502s were undiagnosable.
func TestMessagesUpstreamConnFailurePopulatesErrMsg(t *testing.T) {
	b := New()
	route := callRoute("http://127.0.0.1:1") // port 1: nothing listens, connect fails fast
	call := &backend.Call{
		Raw:      []byte(minimalReq),
		Envelope: anthropic.Envelope{Model: "gpt"},
		Route:    route,
	}
	rec := httptest.NewRecorder()
	res := b.Messages(context.Background(), call, rec)

	if res.Status != 502 {
		t.Fatalf("Status = %d, want 502", res.Status)
	}
	if res.ErrMsg == "" {
		t.Error("ErrMsg is empty; upstream connect failures must be diagnosable in the router log")
	}
	if !strings.Contains(res.ErrMsg, "openai upstream") {
		t.Errorf("ErrMsg = %q, want it to mention the provider", res.ErrMsg)
	}
}

// TestMessagesUpstreamBadJSONPopulatesErrMsg covers the "upstream body
// unparseable" translation-failure path: a 200 response whose body isn't
// valid JSON. Same undiagnosable-502 class as the connect-failure case.
func TestMessagesUpstreamBadJSONPopulatesErrMsg(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("not json"))
	}))
	defer upstream.Close()

	b := New()
	call := &backend.Call{
		Raw:      []byte(minimalReq),
		Envelope: anthropic.Envelope{Model: "gpt"},
		Route:    callRoute(upstream.URL),
	}
	rec := httptest.NewRecorder()
	res := b.Messages(context.Background(), call, rec)

	if res.Status != 502 {
		t.Fatalf("Status = %d, want 502", res.Status)
	}
	if res.ErrMsg == "" {
		t.Error("ErrMsg is empty for an unparseable upstream body")
	}
}
