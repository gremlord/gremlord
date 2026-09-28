package rawjson

import "testing"

func TestSpanNested(t *testing.T) {
	doc := []byte(`{"type":"message_start", "message":{"id":"x","usage":{"input_tokens": 120,"output_tokens":1}},"z":"<b>"}`)
	s, e, ok := Span(doc, "message", "usage", "input_tokens")
	if !ok || string(doc[s:e]) != "120" {
		t.Fatalf("span = %q ok=%v", doc[s:e], ok)
	}
	if _, _, ok := Span(doc, "message", "nope"); ok {
		t.Error("missing key resolved")
	}
	if _, _, ok := Span(doc, "type", "x"); ok {
		t.Error("descended into a string")
	}
	if _, _, ok := Span([]byte(`{"a":`), "a"); ok {
		t.Error("truncated document resolved")
	}
}

func TestSpanLastDuplicateWins(t *testing.T) {
	doc := []byte(`{"model":"auto","safeguards":{"model":"inner"},"model":"auto2"}`)
	s, e, ok := Span(doc, "model")
	if !ok || string(doc[s:e]) != `"auto2"` {
		t.Fatalf("span = %q ok=%v", doc[s:e], ok)
	}
	out, ok := Replace(doc, []byte(`"x"`), "model")
	if !ok || string(out) != `{"model":"auto","safeguards":{"model":"inner"},"model":"x"}` {
		t.Fatalf("got %s", out)
	}
}

func TestSpanSkipsKeysInsideSiblingValues(t *testing.T) {
	// A nested "model" key must not be mistaken for the top-level one.
	doc := []byte(`{"metadata":{"model":"inner"},"model":"outer"}`)
	s, e, ok := Span(doc, "model")
	if !ok || string(doc[s:e]) != `"outer"` {
		t.Fatalf("span = %q ok=%v", doc[s:e], ok)
	}
}

func TestReplacePreservesEverythingElse(t *testing.T) {
	doc := []byte(`{"z":1,"model":"auto",  "safeguards":{"k":"<x>&"},"a":[1,2]}`)
	out, ok := Replace(doc, []byte(`"claude-opus-5-5"`), "model")
	want := `{"z":1,"model":"claude-opus-5-5",  "safeguards":{"k":"<x>&"},"a":[1,2]}`
	if !ok || string(out) != want {
		t.Fatalf("got %s", out)
	}
	if out, ok := Replace(doc, []byte(`1`), "missing"); ok || string(out) != string(doc) {
		t.Error("unresolved replace must return input unchanged")
	}
}

func TestMarshalNoHTMLEscape(t *testing.T) {
	b, err := Marshal(map[string]string{"t": "<system-reminder>&"})
	if err != nil || string(b) != `{"t":"<system-reminder>&"}` {
		t.Fatalf("got %s err=%v", b, err)
	}
}
