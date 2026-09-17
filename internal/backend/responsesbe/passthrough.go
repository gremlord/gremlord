// Package responsesbe preserves native Codex Responses traffic. It observes
// usage without translating tools, reasoning, history, or client token counts.
package responsesbe

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
)

type Backend struct{ client *http.Client }

func New() *Backend {
	return &Backend{client: &http.Client{
		Transport:     backend.NewTransport(),
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

func WriteError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"type": "invalid_request_error", "message": message, "code": "gremlord_error",
	}})
}

// Forward records usage before releasing a terminal event to the client, so
// an immediate tool continuation sees the updated budget. No upstream retries
// are performed here: Codex owns retries, each of which is metered separately.
func (b *Backend) Forward(ctx context.Context, call *backend.Call, w http.ResponseWriter, path string, record func(backend.Result)) {
	res := backend.Result{Status: http.StatusBadGateway}
	recorded := false
	finish := func() {
		if !recorded {
			recorded = true
			res.ReportedInput = res.Usage.InputSide()
			record(res)
		}
	}
	defer finish()
	u := strings.TrimRight(call.Route.Provider.BaseURL, "/") + path
	if query := call.Query.Encode(); query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(call.Raw))
	if err != nil {
		res.ErrType = "invalid_upstream_url"
		WriteError(w, 502, "gremlord: invalid Responses upstream URL")
		return
	}
	copyHeaders(req.Header, call.Header)
	req.Header.Del("Accept-Encoding") // Go decompresses the response for the meter.
	req.Header.Del("Content-Encoding")
	req.Header.Set("Content-Type", "application/json")
	if key := call.Route.Provider.Key(); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		res.ErrType = "upstream_transport_error"
		if ctx.Err() != nil {
			res.Status, res.ErrType = 499, "client_disconnect"
		} else {
			WriteError(w, 502, "gremlord: Responses upstream transport failed")
		}
		return
	}
	defer resp.Body.Close()
	res.Status = resp.StatusCode
	copyHeaders(w.Header(), resp.Header)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		res.ErrType = "upstream_http_error"
		finish()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, io.LimitReader(resp.Body, 1<<20))
		return
	}
	if !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
		if err != nil || len(data) > 64<<20 {
			res.Status, res.ErrType = 502, "upstream_read_error"
			WriteError(w, 502, "gremlord: could not read Responses upstream body")
			return
		}
		observeResponse(data, &res)
		finish()
		w.WriteHeader(resp.StatusCode)
		w.Write(data)
		return
	}
	w.WriteHeader(resp.StatusCode)
	scan := bufio.NewScanner(resp.Body)
	scan.Buffer(make([]byte, 64<<10), 32<<20)
	// Keep the original line endings and bytes, including unknown SSE fields.
	scan.Split(func(data []byte, eof bool) (int, []byte, error) {
		if i := bytes.IndexByte(data, '\n'); i >= 0 {
			return i + 1, data[:i+1], nil
		}
		if eof && len(data) > 0 {
			return len(data), data, nil
		}
		return 0, nil, nil
	})
	var eventData []byte
	terminal := false
	for scan.Scan() {
		line := scan.Bytes()
		text := bytes.TrimRight(line, "\r\n")
		if data, ok := bytes.CutPrefix(text, []byte("data:")); ok {
			data = bytes.TrimPrefix(data, []byte(" "))
			if len(eventData)+len(data) > 32<<20 {
				res.Status, res.ErrType = 502, "upstream_event_too_large"
				return
			}
			eventData = append(eventData, data...)
			eventData = append(eventData, '\n')
		}
		if len(text) == 0 {
			var event struct {
				Type     string          `json:"type"`
				Response json.RawMessage `json:"response"`
			}
			if json.Unmarshal(eventData, &event) == nil {
				switch event.Type {
				case "response.completed", "response.incomplete", "response.failed":
					observeResponse(event.Response, &res)
					if event.Type != "response.completed" && res.ErrType == "" {
						res.ErrType = event.Type
					}
					terminal = true
					finish()
				case "error":
					res.ErrType = "upstream_stream_error"
				}
			}
			eventData = eventData[:0]
		}
		if _, err := w.Write(line); err != nil {
			res.Status, res.ErrType = 499, "client_disconnect"
			return
		}
		if len(text) == 0 {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
	if !terminal {
		res.Status, res.ErrType = 502, "incomplete_stream"
		if ctx.Err() != nil {
			res.Status, res.ErrType = 499, "client_disconnect"
		}
	}
}

func observeResponse(data []byte, res *backend.Result) {
	var response struct {
		Status string `json:"status"`
		Usage  *struct {
			Input   int64 `json:"input_tokens"`
			Output  int64 `json:"output_tokens"`
			Details struct {
				Read  int64 `json:"cached_tokens"`
				Write int64 `json:"cache_write_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(data, &response) != nil {
		res.ErrType = "invalid_upstream_json"
		return
	}
	if response.Usage == nil {
		res.ErrType = "usage_unavailable"
	} else {
		u := response.Usage
		read := min(max(u.Details.Read, 0), max(u.Input, 0))
		write := min(max(u.Details.Write, 0), max(u.Input-read, 0))
		res.Usage = anthropic.Usage{InputTokens: max(u.Input-read-write, 0), OutputTokens: max(u.Output, 0), CacheReadInputTokens: read, CacheCreationInputTokens: write}
	}
	if response.Status == "failed" || response.Status == "incomplete" {
		res.ErrType = "response_" + response.Status
	}
}

func copyHeaders(dst, src http.Header) {
	for key, values := range src {
		dst[key] = append([]string(nil), values...)
	}
	for _, line := range src.Values("Connection") {
		for _, key := range strings.Split(line, ",") {
			dst.Del(strings.TrimSpace(key))
		}
	}
	for key := range dst {
		lower := strings.ToLower(key)
		if strings.HasPrefix(lower, "x-gremlord-") || strings.HasPrefix(lower, "x-agentic-") {
			dst.Del(key)
		}
	}
	for _, key := range []string{"Authorization", "X-Api-Key", "Cookie", "Set-Cookie", "Content-Length", "Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate", "Proxy-Authorization", "TE", "Trailer", "Transfer-Encoding", "Upgrade"} {
		dst.Del(key)
	}
}
