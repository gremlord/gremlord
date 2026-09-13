package anthropic

import (
	"encoding/json"
	"fmt"
)

// MessagesRequest is the parsed shape of a /v1/messages body, used by the
// translation backend. Passthrough never parses this deeply.
type MessagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	System        SystemPrompt    `json:"system,omitempty"`
	Messages      []Message       `json:"messages"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Stream        bool            `json:"stream,omitempty"`
	Thinking      *Thinking       `json:"thinking,omitempty"`
	Metadata      json.RawMessage `json:"metadata,omitempty"`
}

func ParseRequest(raw []byte) (*MessagesRequest, error) {
	var req MessagesRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, fmt.Errorf("parsing messages request: %w", err)
	}
	return &req, nil
}

// SystemPrompt accepts both the string form and the block-array form.
type SystemPrompt []ContentBlock

func (s *SystemPrompt) UnmarshalJSON(data []byte) error {
	var str string
	if json.Unmarshal(data, &str) == nil {
		*s = SystemPrompt{{Type: "text", Text: str}}
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	*s = blocks
	return nil
}

// Text concatenates the prompt's text blocks.
func (s SystemPrompt) Text() string {
	out := ""
	for _, b := range s {
		if b.Type == "text" {
			if out != "" {
				out += "\n\n"
			}
			out += b.Text
		}
	}
	return out
}

type Message struct {
	Role    string      `json:"role"`
	Content MessageBody `json:"content"`
}

// MessageBody accepts both the string form and the block-array form.
type MessageBody []ContentBlock

func (m *MessageBody) UnmarshalJSON(data []byte) error {
	var str string
	if json.Unmarshal(data, &str) == nil {
		*m = MessageBody{{Type: "text", Text: str}}
		return nil
	}
	var blocks []ContentBlock
	if err := json.Unmarshal(data, &blocks); err != nil {
		return err
	}
	*m = blocks
	return nil
}

// ContentBlock is the tagged union of Messages API content blocks; only
// the fields for the given Type are populated.
type ContentBlock struct {
	Type string `json:"type"`

	// text / thinking
	Text     string `json:"text,omitempty"`
	Thinking string `json:"thinking,omitempty"`

	// image
	Source *ImageSource `json:"source,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result
	ToolUseID string      `json:"tool_use_id,omitempty"`
	Content   MessageBody `json:"content,omitempty"`
	IsError   bool        `json:"is_error,omitempty"`

	// tool_reference — a deferred tool the client just pulled in
	ToolName string `json:"tool_name,omitempty"`
}

// FlatText renders a tool_result's content (string or blocks) as text.
// Nested images become an omission marker — translators that can hoist
// them (openaibe) should use ContentText + ImageSources instead.
func (b ContentBlock) FlatText() string {
	return b.contentText(true)
}

// ContentText is FlatText with image blocks skipped rather than replaced
// by an omission marker. Pair with ImageSources when the translator can
// forward those pixels as a trailing user image message.
func (b ContentBlock) ContentText() string {
	return b.contentText(false)
}

func (b ContentBlock) contentText(markImages bool) string {
	out := ""
	for _, c := range b.Content {
		switch c.Type {
		case "text":
			if out != "" {
				out += "\n"
			}
			out += c.Text
		case "image":
			if !markImages {
				continue
			}
			out += fmt.Sprintf("\n[image omitted from tool result %s]", b.ToolUseID)
		case "tool_reference":
			// Deferred tool loading: Claude Code answers its own ToolSearch
			// call with bare references and the Anthropic API expands them
			// into schemas. A translated backend has no such expansion, and
			// dropping the block would leave the tool result empty — which
			// some OpenAI-dialect servers reject outright. Say what happened
			// instead; the client puts the real schema in the next request's
			// tools array either way.
			if out != "" {
				out += "\n"
			}
			if c.ToolName == "" {
				out += "Tool schema loaded."
				continue
			}
			out += "Tool schema loaded: " + c.ToolName
		}
	}
	return out
}

// ImageSources returns nested image sources from a tool_result's content.
func (b ContentBlock) ImageSources() []*ImageSource {
	var out []*ImageSource
	for _, c := range b.Content {
		if c.Type == "image" && c.Source != nil {
			out = append(out, c.Source)
		}
	}
	return out
}

type ImageSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// DataURL is the OpenAI-shaped image URL: a data: URI for base64 sources,
// otherwise the remote URL.
func (s *ImageSource) DataURL() string {
	if s == nil {
		return ""
	}
	if s.Type == "base64" {
		return fmt.Sprintf("data:%s;base64,%s", s.MediaType, s.Data)
	}
	return s.URL
}

// Tool is a user-defined tool. Anthropic server tools carry a versioned
// Type and no InputSchema; those cannot translate to OpenAI.
type Tool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

func (t Tool) IsServerTool() bool {
	return len(t.InputSchema) == 0 && t.Type != "" && t.Type != "custom"
}

type ToolChoice struct {
	Type                   string `json:"type"`
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

type Thinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// Response types (built by the translation backend).

type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // "message"
	Role         string         `json:"role"` // "assistant"
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason,omitempty"`
	StopSequence *string        `json:"stop_sequence"`
	Usage        Usage          `json:"usage"`
}
