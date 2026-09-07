// Package anthropicbe forwards requests to the Anthropic API byte-faithfully,
// teeing usage out of the response.
package anthropicbe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/tokens"
)

type Backend struct {
	client *http.Client
}

func New() *Backend {
	return &Backend{client: &http.Client{Transport: backend.NewTransport()}}
}

func (b *Backend) Messages(ctx context.Context, call *backend.Call, w http.ResponseWriter) backend.Result {
	return b.forward(ctx, call, w, "/v1/messages", true)
}

func (b *Backend) CountTokens(ctx context.Context, call *backend.Call, w http.ResponseWriter) backend.Result {
	return b.forward(ctx, call, w, "/v1/messages/count_tokens", false)
}

func (b *Backend) forward(ctx context.Context, call *backend.Call, w http.ResponseWriter, path string, tee bool) backend.Result {
	body := call.Raw
	// Byte-faithful when the alias already is the upstream model id —
	// cache_control, thinking blocks, and unknown future fields survive. The
	// exceptions are poisoned history from a translated backend: invalid
	// tool ids (e.g. vLLM's "Bash:0") and unsigned display-only thinking.
	// Those requests must be parsed and repaired before Anthropic rejects them.
	if call.Envelope.Model != call.Route.Model.ID || hasInvalidToolID(call.Raw) || hasUnsignedThinking(call.Raw) {
		var err error
		body, err = rewriteForModel(call.Raw, call.Route.Model.ID)
		if err != nil {
			anthropic.WriteError(w, 400, "invalid_request_error", "gremlord: could not parse request body: "+err.Error())
			return backend.Result{Status: 400, ErrType: "invalid_request_error"}
		}
	}

	u := strings.TrimSuffix(call.Route.Provider.BaseURL, "/") + path
	if q := call.Query.Encode(); q != "" {
		u += "?" + q
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		anthropic.WriteError(w, 500, "api_error", "gremlord: "+err.Error())
		return backend.Result{Status: 500, ErrType: "api_error"}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", call.Route.Provider.Key())
	// Join rather than Get: a repeated anthropic-beta line would otherwise
	// lose every value but the first, and these are load-bearing. Dropping
	// advanced-tool-use-2025-11-20 while the conversation carries
	// tool_reference blocks earns a 400 that persists for the rest of the
	// session, since the blocks stay in history.
	for _, h := range []string{"anthropic-version", "anthropic-beta"} {
		if v := strings.Join(call.Header.Values(h), ","); v != "" {
			req.Header.Set(h, v)
		}
	}
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", "2023-06-01")
	}

	resp, err := b.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return backend.Result{Status: 499, ErrType: "client_disconnect"}
		}
		anthropic.WriteError(w, 500, "api_error", fmt.Sprintf("anthropic upstream: %v", err))
		return backend.Result{Status: 502, ErrType: "api_error"}
	}
	defer resp.Body.Close()

	// Upstream responses (including errors) are already Anthropic-shaped;
	// pass status, content headers, and body through unchanged.
	for _, h := range []string{"Content-Type", "Cache-Control", "anthropic-ratelimit-requests-remaining", "retry-after", "request-id"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	res := backend.Result{Status: resp.StatusCode}
	if resp.StatusCode >= 400 {
		res.ErrType = anthropic.ErrorTypeForStatus(resp.StatusCode)
	}
	if !tee || resp.StatusCode >= 400 {
		buf, _ := io.ReadAll(resp.Body)
		w.Write(buf)
		if resp.StatusCode >= 400 {
			res.ErrMsg = errSnippet(buf)
		}
		return res
	}

	// Usually 1 — see scale.go for the two cases that turn scaling on.
	factor := tokens.ScaleFactor(call.ScaleBudget())

	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		res.Usage = copySSEWithUsageTee(resp.Body, w, factor)
		res.ReportedInput = tokens.ScaleUsage(res.Usage, factor).InputSide()
		return res
	}

	buf, err := io.ReadAll(resp.Body)
	if err == nil {
		var parsed struct {
			Usage anthropic.Usage `json:"usage"`
		}
		json.Unmarshal(buf, &parsed)
		res.Usage = parsed.Usage
		res.ReportedInput = tokens.ScaleUsage(res.Usage, factor).InputSide()
		buf = scaleResponseBody(buf, factor)
	}
	w.Write(buf)
	return res
}

// copySSEWithUsageTee streams the SSE body through while watching data:
// lines for message_start (input + cache tokens) and message_delta
// (output tokens). Flushes after every line so streaming stays live. The
// usage returned is always TRUE upstream usage; factor rewrites only what
// the client sees, and at factor 1 (the common case) every line is
// forwarded byte-for-byte.
func copySSEWithUsageTee(r io.Reader, w http.ResponseWriter, factor float64) anthropic.Usage {
	var usage anthropic.Usage
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if data, ok := bytes.CutPrefix(line, []byte("data: ")); ok {
			scanUsage(data, &usage)
			if scaled := scaleSSEData(data, factor); !bytes.Equal(scaled, data) {
				line = append([]byte("data: "), scaled...)
			}
		}
		w.Write(line)
		w.Write([]byte("\n"))
		if flusher != nil {
			flusher.Flush()
		}
	}
	return usage
}

// errSnippet extracts a short error message from an Anthropic error body.
func errSnippet(body []byte) string {
	var apiErr anthropic.APIError
	msg := ""
	if json.Unmarshal(body, &apiErr) == nil && apiErr.Error.Message != "" {
		msg = apiErr.Error.Message
	} else {
		msg = strings.TrimSpace(string(body))
	}
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

func scanUsage(data []byte, usage *anthropic.Usage) {
	// Cheap pre-filter before JSON parsing.
	if !bytes.Contains(data, []byte(`"usage"`)) {
		return
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage anthropic.Usage `json:"usage"`
		} `json:"message"`
		Usage anthropic.Usage `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return
	}
	switch ev.Type {
	case "message_start":
		usage.InputTokens = ev.Message.Usage.InputTokens
		usage.CacheReadInputTokens = ev.Message.Usage.CacheReadInputTokens
		usage.CacheCreationInputTokens = ev.Message.Usage.CacheCreationInputTokens
	case "message_delta":
		if ev.Usage.OutputTokens > 0 {
			usage.OutputTokens = ev.Usage.OutputTokens
		}
	}
}
