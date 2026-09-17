package router

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/gremlord/gremlord/internal/backend"
	"github.com/gremlord/gremlord/internal/backend/responsesbe"
	"github.com/gremlord/gremlord/internal/config"
	"github.com/gremlord/gremlord/internal/tokens"
	"github.com/gremlord/gremlord/internal/wire"
)

func (s *Server) handleResponses(compact bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		fail := func(status int, msg string) { responsesbe.WriteError(w, status, "gremlord: "+msg) }
		if encoding := r.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
			fail(415, "native Responses PoC requires uncompressed requests")
			return
		}
		raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<20))
		var body map[string]json.RawMessage
		if err != nil || json.Unmarshal(raw, &body) != nil || body == nil {
			fail(400, "invalid Responses JSON body (maximum 64 MiB)")
			return
		}
		var model string
		if json.Unmarshal(body["model"], &model) != nil || model == "" {
			fail(400, "Responses requires a model")
			return
		}
		alias := model
		if pin := wire.PinModel(r.Header); pin != "" {
			alias = pin
		}
		cfg := s.cfg.Load()
		if _, dynamic := cfg.Routing[alias]; dynamic {
			fail(400, "Codex PoC requires a fixed Responses model alias; automatic routing is not supported")
			return
		}
		route, err := cfg.Resolve(alias)
		if err != nil {
			fail(404, err.Error())
			return
		}
		if route.Provider.Type != config.ProviderOpenAI || route.APIFlavor() != config.APIResponses {
			fail(400, "Codex PoC requires an OpenAI Responses alias; reverse translation is not implemented")
			return
		}
		if tooLarge, size, cap := bodyTooLarge(route, int64(len(raw))); tooLarge {
			fail(400, fmt.Sprintf("request body %d bytes exceeds provider max_request_bytes %d", size, cap))
			return
		}
		for _, field := range []string{"background", "store"} {
			var enabled bool
			if value, ok := body[field]; ok && (string(value) == "null" || json.Unmarshal(value, &enabled) != nil || enabled) {
				fail(400, field+" must be false in the native Responses PoC")
				return
			}
		}
		for _, field := range []string{"previous_response_id", "conversation"} {
			if value := body[field]; len(value) > 0 && string(value) != "null" {
				fail(400, "Codex PoC requires full client history; "+field+" is unsupported")
				return
			}
		}
		changed := false
		if !compact && len(body["store"]) == 0 {
			body["store"] = json.RawMessage("false")
			changed = true
		}
		if model != route.Model.ID {
			body["model"], _ = json.Marshal(route.Model.ID)
			changed = true
		}
		if !compact {
			if cap := route.Model.MaxOutput; cap > 0 {
				var requested int
				if value, ok := body["max_output_tokens"]; ok && string(value) != "null" {
					if json.Unmarshal(value, &requested) != nil || requested <= 0 {
						fail(400, "invalid max_output_tokens")
						return
					}
				}
				if requested == 0 || requested > cap {
					body["max_output_tokens"], _ = json.Marshal(cap)
					changed = true
				}
			}
			if effort := route.Model.ReasoningEffort; effort != "" {
				reasoning := map[string]json.RawMessage{}
				if value := body["reasoning"]; len(value) > 0 && string(value) != "null" {
					if json.Unmarshal(value, &reasoning) != nil {
						fail(400, "invalid reasoning object")
						return
					}
				}
				reasoning["effort"], _ = json.Marshal(effort)
				body["reasoning"], _ = json.Marshal(reasoning)
				changed = true
			}
		}
		if changed {
			raw, _ = json.Marshal(body)
		}
		if tooLarge, _, cap := bodyTooLarge(route, int64(len(raw))); tooLarge {
			fail(400, fmt.Sprintf("rewritten request exceeds provider max_request_bytes %d", cap))
			return
		}
		if msg := s.gate.Check(wire.Profile(r.Header)); msg != "" {
			fail(400, msg)
			return
		}
		path := "/responses"
		if compact {
			path += "/compact"
		}
		s.responses.Forward(r.Context(), &backend.Call{Raw: raw, Route: route, Header: r.Header, Query: r.URL.Query()}, w, path, func(res backend.Result) {
			s.recordUsage(r, route, alias, res, time.Since(start), tokens.Composition{}, 0)
		})
	}
}
