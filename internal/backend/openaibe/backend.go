package openaibe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gremlord/gremlord/internal/anthropic"
	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/openai"
	"github.com/gremlord/gremlord/internal/tokens"
)

type Backend struct {
	client *http.Client
}

func New() *Backend {
	return &Backend{client: &http.Client{Transport: backend.NewTransport()}}
}

func (b *Backend) Messages(ctx context.Context, call *backend.Call, w http.ResponseWriter) backend.Result {
	req, err := anthropic.ParseRequest(call.Raw)
	if err != nil {
		anthropic.WriteError(w, 400, "invalid_request_error", "gremlord: "+err.Error())
		return backend.Result{Status: 400, ErrType: "invalid_request_error"}
	}
	responses := call.Route.APIFlavor() == config.APIResponses
	var body []byte
	if responses {
		rr, err := TranslateResponsesRequest(req, call.Route)
		if err != nil {
			anthropic.WriteError(w, 400, "invalid_request_error", "gremlord translate: "+err.Error())
			return backend.Result{Status: 400, ErrType: "invalid_request_error"}
		}
		body, err = json.Marshal(rr)
		if err != nil {
			anthropic.WriteError(w, 500, "api_error", "gremlord: "+err.Error())
			return backend.Result{Status: 500, ErrType: "api_error"}
		}
	} else {
		chatReq, err := TranslateRequest(req, call.Route)
		if err != nil {
			anthropic.WriteError(w, 400, "invalid_request_error", "gremlord translate: "+err.Error())
			return backend.Result{Status: 400, ErrType: "invalid_request_error"}
		}
		body, err = json.Marshal(chatReq)
		if err != nil {
			anthropic.WriteError(w, 500, "api_error", "gremlord: "+err.Error())
			return backend.Result{Status: 500, ErrType: "api_error"}
		}
	}

	path := "/chat/completions"
	if responses {
		path = "/responses"
	}
	u := strings.TrimSuffix(call.Route.Provider.BaseURL, "/") + path
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		anthropic.WriteError(w, 500, "api_error", "gremlord: "+err.Error())
		return backend.Result{Status: 500, ErrType: "api_error"}
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if key := call.Route.Provider.Key(); key != "" {
		httpReq.Header.Set("Authorization", "Bearer "+key)
	}

	resp, err := b.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return backend.Result{Status: 499, ErrType: "client_disconnect"}
		}
		msg := fmt.Sprintf("%s upstream: %v", call.Route.ProviderName, err)
		anthropic.WriteError(w, 500, "api_error", msg)
		return backend.Result{Status: 502, ErrType: "api_error", ErrMsg: msg}
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		return writeUpstreamError(w, resp, call.Route.ProviderName, call.Route.Model.ID)
	}

	scale := tokens.ScaleFactor(call.ScaleBudget())

	if req.Stream {
		sse := anthropic.NewSSEWriter(w)
		state := newStreamState(sse, call.Envelope.Model)
		state.scale = scale
		state.estInput = tokens.ScaleCount(call.EstimateInput(req), scale)
		var usage anthropic.Usage
		var errType string
		if responses {
			usage, errType = state.RunResponses(ctx, resp.Body)
		} else {
			usage, errType = state.Run(ctx, resp.Body)
		}
		status := 200
		switch errType {
		case "client_disconnect":
			status = 499
		case "api_error":
			status = 502
		}
		return backend.Result{Status: status, Usage: usage, ErrType: errType,
			ReportedInput: tokens.ScaleUsage(usage, scale).InputSide()}
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		msg := "gremlord: reading upstream body: " + err.Error()
		anthropic.WriteError(w, 500, "api_error", msg)
		return backend.Result{Status: 502, ErrType: "api_error", ErrMsg: msg}
	}
	var out *anthropic.MessagesResponse
	if responses {
		var parsed openai.ResponsesResponse
		if err := json.Unmarshal(raw, &parsed); err != nil {
			msg := "gremlord: upstream body unparseable: " + err.Error()
			anthropic.WriteError(w, 500, "api_error", msg)
			return backend.Result{Status: 502, ErrType: "api_error", ErrMsg: msg}
		}
		out, err = TranslateResponsesResponse(&parsed, call.Envelope.Model)
	} else {
		var parsed openai.ChatResponse
		if err := json.Unmarshal(raw, &parsed); err != nil {
			msg := "gremlord: upstream body unparseable: " + err.Error()
			anthropic.WriteError(w, 500, "api_error", msg)
			return backend.Result{Status: 502, ErrType: "api_error", ErrMsg: msg}
		}
		out, err = TranslateResponse(&parsed, call.Envelope.Model)
	}
	if err != nil {
		msg := "gremlord translate: " + err.Error()
		anthropic.WriteError(w, 500, "api_error", msg)
		return backend.Result{Status: 502, ErrType: "api_error", ErrMsg: msg}
	}
	trueUsage := out.Usage
	out.Usage = tokens.ScaleUsage(trueUsage, scale)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
	return backend.Result{Status: 200, Usage: trueUsage, ReportedInput: out.Usage.InputSide()}
}

// CountTokens has no OpenAI-dialect equivalent — serve a local estimate,
// scaled to the model's context budget so Claude Code's auto-compact
// threshold tracks the real window.
func (b *Backend) CountTokens(ctx context.Context, call *backend.Call, w http.ResponseWriter) backend.Result {
	req, err := anthropic.ParseRequest(call.Raw)
	if err != nil {
		anthropic.WriteError(w, 400, "invalid_request_error", "gremlord: "+err.Error())
		return backend.Result{Status: 400, ErrType: "invalid_request_error"}
	}
	n := tokens.ScaleCount(call.EstimateInput(req), tokens.ScaleFactor(call.ScaleBudget()))
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(anthropic.CountTokensResponse{InputTokens: n})
	return backend.Result{Status: 200}
}

func writeUpstreamError(w http.ResponseWriter, resp *http.Response, provider, model string) backend.Result {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	msg := strings.TrimSpace(string(raw))
	var oaiErr struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &oaiErr) == nil && oaiErr.Error.Message != "" {
		msg = oaiErr.Error.Message
	}
	if resp.StatusCode == 404 {
		msg = fmt.Sprintf("model %q not found on provider %q: %s", model, provider, msg)
	}
	errType := anthropic.ErrorTypeForStatus(resp.StatusCode)
	if ra := resp.Header.Get("retry-after"); ra != "" {
		w.Header().Set("retry-after", ra)
	}
	anthropic.WriteError(w, resp.StatusCode, errType, fmt.Sprintf("%s: %s", provider, msg))
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return backend.Result{Status: resp.StatusCode, ErrType: errType, ErrMsg: msg}
}
