// Package tokens estimates input token counts for backends without a
// count_tokens endpoint. Deliberately biased HIGH: Claude Code uses this
// for the auto-compact threshold, and compacting early is harmless while
// blowing the context window is fatal.
//
// The bias is not free, though: an over-count reserves window the model
// could have used. Calibration (see calibrate.go) measures the raw
// estimate against real upstream usage and shrinks the gap without
// giving up the safety property.
package tokens

import (
	"unicode/utf8"

	"github.com/gremlord/gremlord/internal/anthropic"
)

const (
	charsPerToken      = 3.5
	perImageTokens     = 1500
	perMessageOverhead = 6
	safetyMargin       = 1.10
)

// Composition splits an estimate into the three parts of a Messages
// request that grow independently. System and Tools are the fixed tax
// paid on every request in a session; Messages is the part that actually
// accumulates. Recorded per request so "what is my context actually
// spent on" is a query, not a guess.
type Composition struct {
	System   int64
	Tools    int64
	Messages int64
}

func (c Composition) Total() int64 { return c.System + c.Tools + c.Messages }

// Compose estimates each section of a request separately. The sum is
// exactly what Estimate returns.
func Compose(req *anthropic.MessagesRequest) Composition {
	if req == nil {
		return Composition{}
	}

	sysChars := 0
	for _, b := range req.System {
		sysChars += utf8.RuneCountInString(b.Text)
	}

	toolChars := 0
	for _, t := range req.Tools {
		toolChars += utf8.RuneCountInString(t.Name) + utf8.RuneCountInString(t.Description) + len(t.InputSchema)
	}

	msgChars, images := 0, 0
	for _, m := range req.Messages {
		for _, b := range m.Content {
			msgChars += utf8.RuneCountInString(b.Text)
			msgChars += utf8.RuneCountInString(b.Thinking)
			msgChars += len(b.Input)
			if b.Type == "image" {
				images++
			}
			for _, inner := range b.Content { // tool_result content
				msgChars += utf8.RuneCountInString(inner.Text)
				if inner.Type == "image" {
					images++
				}
			}
		}
	}
	msgTokens := float64(msgChars)/charsPerToken +
		float64(images*perImageTokens) +
		float64(len(req.Messages)*perMessageOverhead)

	return Composition{
		System:   int64(float64(sysChars) / charsPerToken * safetyMargin),
		Tools:    int64(float64(toolChars) / charsPerToken * safetyMargin),
		Messages: int64(msgTokens * safetyMargin),
	}
}

// Estimate returns the bias-high input estimate for a request.
func Estimate(req *anthropic.MessagesRequest) int64 { return Compose(req).Total() }
