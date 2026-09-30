package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/sofired/berth/internal/text"
)

// jsonKV is an ordered JSON object. The override document is built the way
// a JavaScript object spread and Object.fromEntries would build it, whose
// key order is insertion order, and JSON.stringify preserves it; Go's map
// does not. jsonKV is this package's ordered map, for both decoding `docker
// compose config`'s output (see decodeOrdered) and building the override
// document with that same key order (see overrideDocument in
// compose_up.go).
//
// set behaves like a JavaScript property assignment: a key already present
// keeps its position and only its value changes; a new key is appended.
// That is exactly what the override document's own construction relies on —
// see a routed service's networks entry merging onto its existing ones, and
// a router network that happens to collide with an already-declared one.
type jsonKV struct {
	keys   []string
	values map[string]any
}

// newJSONKV is an empty ordered object.
func newJSONKV() *jsonKV { return &jsonKV{values: map[string]any{}} }

// set assigns key, exactly as a JavaScript property assignment would: a key
// already present keeps its position and only its value changes; a new key
// is appended. Returns o, so a value can be built up in a chain of calls.
func (o *jsonKV) set(key string, value any) *jsonKV {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
	return o
}

// get is key's value, or false if it is not set. A nil receiver reads as not
// set, so a lookup on an absent optional section needs no nil check first.
func (o *jsonKV) get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.values[key]
	return v, ok
}

// has reports whether key is set.
func (o *jsonKV) has(key string) bool {
	_, ok := o.get(key)
	return ok
}

// decodeOrdered decodes data into a tree of *jsonKV (objects), []any
// (arrays), json.Number, string, bool, and nil (null) — preserving every
// object's key order exactly as it appeared in data. This is what lets the
// override document follow the model's own service, volume, and network
// order, the same order JSON.parse followed by Object.entries would see.
func decodeOrdered(data []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	value, err := decodeOrderedValue(decoder)
	if err != nil {
		return nil, err
	}
	if decoder.More() {
		return nil, fmt.Errorf("backend: trailing data after a JSON value")
	}
	return value, nil
}

// decodeOrderedValue is the next complete JSON value off decoder: a scalar,
// or an object or array read recursively by decodeOrderedToken.
func decodeOrderedValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	return decodeOrderedToken(decoder, token)
}

// decodeOrderedToken finishes decoding the value token already started:
// itself, if it is a scalar, or the rest of the object or array it opens,
// with every object built as a *jsonKV in its own key order.
func decodeOrderedToken(decoder *json.Decoder, token json.Token) (any, error) {
	delim, isDelim := token.(json.Delim)
	if !isDelim {
		return token, nil // nil, bool, json.Number, or string
	}
	switch delim {
	case '{':
		obj := newJSONKV()
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyToken.(string)
			if !ok {
				return nil, fmt.Errorf("backend: expected an object key, got %v", keyToken)
			}
			value, err := decodeOrderedValue(decoder)
			if err != nil {
				return nil, err
			}
			obj.set(key, value)
		}
		if _, err := decoder.Token(); err != nil { // consume '}'
			return nil, err
		}
		return obj, nil
	case '[':
		var array []any
		for decoder.More() {
			value, err := decodeOrderedValue(decoder)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if _, err := decoder.Token(); err != nil { // consume ']'
			return nil, err
		}
		return array, nil
	default:
		return nil, fmt.Errorf("backend: unexpected JSON delimiter %v", delim)
	}
}

// marshalOrdered writes value as compact JSON with every jsonKV in its own
// set order and no HTML escaping — matching JSON.stringify exactly, unlike
// encoding/json's own map handling (alphabetical keys) and default escaping
// of '<', '>' and '&'.
func marshalOrdered(value any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeOrdered(&buf, value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// writeOrdered writes value's compact JSON encoding to buf: marshalOrdered's
// own recursive step, one *jsonKV, string, []string, bool or nil at a time.
func writeOrdered(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case string:
		writeJSONString(buf, v)
	case *jsonKV:
		return writeOrderedObject(buf, v)
	case []string:
		return writeOrderedArray(buf, v)
	case []any:
		return writeOrderedItems(buf, v)
	case int:
		buf.WriteString(strconv.Itoa(v))
	default:
		return fmt.Errorf("backend: unsupported value type %T in override document", value)
	}
	return nil
}

// writeOrderedObject writes o to buf as a compact JSON object, its keys in o's own set order.
func writeOrderedObject(buf *bytes.Buffer, o *jsonKV) error {
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeJSONString(buf, key)
		buf.WriteByte(':')
		if err := writeOrdered(buf, o.values[key]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// writeOrderedArray writes items to buf as a compact JSON array of strings —
// the one array shape the override document ever builds itself: a routed
// service's network aliases (RouterNetworkName's own network entry).
func writeOrderedArray(buf *bytes.Buffer, items []string) error {
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeJSONString(buf, item)
	}
	buf.WriteByte(']')
	return nil
}

// writeOrderedItems writes items to buf as a compact JSON array of any value
// writeOrdered supports — the shape of a published-port list.
func writeOrderedItems(buf *bytes.Buffer, items []any) error {
	buf.WriteByte('[')
	for i, item := range items {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeOrdered(buf, item); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

// writeJSONString quotes s exactly as JSON.stringify does — see
// text.JSONString, the one shared quoter every JSON writer in this codebase
// that needs this exact escaping uses. Not encoding/json: even with
// SetEscapeHTML(false), it still escapes U+2028 and U+2029 unconditionally,
// which JSON.stringify writes raw, and a route or a branch name can carry
// either.
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteString(text.JSONString(s))
}
