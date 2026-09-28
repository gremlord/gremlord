// Package rawjson edits JSON documents in place: it swaps a single value
// by byte offset and leaves every other byte — key order, whitespace,
// escaping, fields gremlord has never heard of — exactly as received.
//
// Decoding to map[string]any and re-marshalling is semantically lossless
// but not byte-faithful: keys come back sorted and <, >, & come back as
// < escapes. Claude Code's gateway contract asks for pass-through
// "unchanged" (safeguards on the way up, safeguard_results on the way
// down), so edits that only need to touch one value use this instead.
package rawjson

import (
	"bytes"
	"encoding/json"
	"errors"
)

var errNotObject = errors.New("rawjson: not an object")

// Span returns the byte range [start, end) of the value at path, where
// each path element is an object key. The last occurrence of a duplicated
// key wins, matching encoding/json's last-value-wins decode — the two must
// agree, because callers compare a decoded map against a spliced body.
// ok is false when the document is not valid JSON along the path or a key
// is absent.
func Span(data []byte, path ...string) (start, end int, ok bool) {
	if len(path) == 0 {
		return 0, 0, false
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	base := 0
	for depth, key := range path {
		s, e, err := findKey(dec, key)
		if err != nil {
			return 0, 0, false
		}
		if depth == len(path)-1 {
			return base + s, base + e, true
		}
		// Re-scan inside the matched value. Offsets from the fresh decoder
		// are relative to the sub-slice, so carry the base forward.
		base += s
		dec = json.NewDecoder(bytes.NewReader(data[base : base+(e-s)]))
	}
	return 0, 0, false
}

// findKey walks one object from the decoder's current position and returns
// the offsets of key's value relative to the decoder's input.
func findKey(dec *json.Decoder, key string) (start, end int, err error) {
	tok, err := dec.Token()
	if err != nil {
		return 0, 0, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return 0, 0, errNotObject
	}
	start, found := -1, 0
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return 0, 0, err
		}
		k, _ := tok.(string)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return 0, 0, err
		}
		if k == key {
			// RawMessage holds the value's exact bytes, and InputOffset
			// sits just past them. Keep scanning: a later duplicate
			// overrides this match.
			end := int(dec.InputOffset())
			start, found = end-len(raw), end
		}
	}
	if start < 0 {
		return 0, 0, errors.New("rawjson: key not found")
	}
	return start, found, nil
}

// Replace returns data with the value at path swapped for value, which
// must already be encoded JSON. ok is false (and data is returned as-is)
// when the path does not resolve.
func Replace(data, value []byte, path ...string) ([]byte, bool) {
	s, e, ok := Span(data, path...)
	if !ok {
		return data, false
	}
	out := make([]byte, 0, len(data)-(e-s)+len(value))
	out = append(out, data[:s]...)
	out = append(out, value...)
	return append(out, data[e:]...), true
}

// Marshal encodes v like json.Marshal but without HTML escaping, so text
// containing <system-reminder> round-trips as written.
func Marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
