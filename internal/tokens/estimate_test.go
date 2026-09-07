package tokens

import (
	"encoding/json"
	"testing"

	"github.com/gremlord/gremlord/internal/anthropic"
)

func req(t *testing.T, body string) *anthropic.MessagesRequest {
	t.Helper()
	r, err := anthropic.ParseRequest([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// Compose is what Estimate is made of; if they drift, the composition
// recorded per request stops describing the number the router routes on.
func TestComposeSumsToEstimate(t *testing.T) {
	r := req(t, `{"model":"m","max_tokens":100,
  "system":[{"type":"text","text":"you are a helpful coding agent"}],
  "tools":[{"name":"Bash","description":"run a shell command","input_schema":{"type":"object"}}],
  "messages":[{"role":"user","content":[{"type":"text","text":"hello there"}]}]}`)
	c := Compose(r)
	if got := Estimate(r); got != c.Total() {
		t.Errorf("Estimate() = %d, Compose().Total() = %d", got, c.Total())
	}
	if c.System == 0 || c.Tools == 0 || c.Messages == 0 {
		t.Errorf("every section should be attributed: %+v", c)
	}
}

// Tool schemas are the fixed per-request tax the composition exists to
// expose, so they must land in Tools and nowhere else.
func TestComposeAttributesToolSchemas(t *testing.T) {
	schema, _ := json.Marshal(map[string]any{"type": "object", "properties": map[string]any{
		"command": map[string]any{"type": "string", "description": "the command to run, at some length"},
	}})
	bare := req(t, `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	withTool := req(t, `{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}],
  "tools":[{"name":"Bash","description":"run a shell command","input_schema":`+string(schema)+`}]}`)

	base, tooled := Compose(bare), Compose(withTool)
	if base.Tools != 0 {
		t.Errorf("no tools declared, got Tools=%d", base.Tools)
	}
	if tooled.Tools == 0 {
		t.Error("tool schema not attributed to Tools")
	}
	if tooled.Messages != base.Messages || tooled.System != base.System {
		t.Errorf("adding a tool moved non-tool sections: %+v vs %+v", tooled, base)
	}
}

func TestComposeNilRequest(t *testing.T) {
	if got := Compose(nil).Total(); got != 0 {
		t.Errorf("Compose(nil).Total() = %d, want 0", got)
	}
}
