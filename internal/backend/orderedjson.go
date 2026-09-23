package backend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// jsonKV is an ordered JSON object. TypeScript builds the override document
// with object spreads and Object.fromEntries, whose key order is insertion
// order, and JSON.stringify preserves it; Go's map does not. jsonKV is this
// package's ordered map, for both decoding `docker compose config`'s output
// (see decodeOrdered) and building the override document that must match
// TypeScript's bytes exactly (see overrideDocument in compose_up.go).
//
// set behaves like a JavaScript property assignment: a key already present
// keeps its position and only its value changes; a new key is appended.
// That is exactly what the override document's own construction relies on —
// see routeLabels merging onto a service's existing labels, and a router
// network that happens to collide with an already-declared one.
type jsonKV struct {
	keys   []string
	values map[string]any
}

func newJSONKV() *jsonKV { return &jsonKV{values: map[string]any{}} }

func (o *jsonKV) set(key string, value any) *jsonKV {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
	return o
}

func (o *jsonKV) get(key string) (any, bool) {
	if o == nil {
		return nil, false
	}
	v, ok := o.values[key]
	return v, ok
}

func (o *jsonKV) has(key string) bool {
	_, ok := o.get(key)
	return ok
}

// decodeOrdered decodes data into a tree of *jsonKV (objects), []any
// (arrays), json.Number, string, bool, and nil (null) — preserving every
// object's key order exactly as it appeared in data. This is what lets the
// override document follow the model's own service, volume, and network
// order, matching what JSON.parse followed by Object.entries would see in
// the TypeScript build.
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

func decodeOrderedValue(decoder *json.Decoder) (any, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, err
	}
	return decodeOrderedToken(decoder, token)
}

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
// set order and no HTML escaping — matching TypeScript's JSON.stringify
// exactly, unlike encoding/json's own map handling (alphabetical keys) and
// default escaping of '<', '>' and '&'.
func marshalOrdered(value any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeOrdered(&buf, value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

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
		return writeJSONString(buf, v)
	case *jsonKV:
		return writeOrderedObject(buf, v)
	default:
		return fmt.Errorf("backend: unsupported value type %T in override document", value)
	}
	return nil
}

func writeOrderedObject(buf *bytes.Buffer, o *jsonKV) error {
	buf.WriteByte('{')
	for i, key := range o.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeJSONString(buf, key); err != nil {
			return err
		}
		buf.WriteByte(':')
		if err := writeOrdered(buf, o.values[key]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// writeJSONString quotes s the way JSON.stringify does: encoding/json's own
// string quoting (correct Unicode and control-character escaping), minus
// the HTML escaping it adds by default — the same rule files.AtomicJSON and
// text.SafeJSON apply, here per string since jsonKV has no single Encoder to
// configure.
func writeJSONString(buf *bytes.Buffer, s string) error {
	var scratch bytes.Buffer
	encoder := json.NewEncoder(&scratch)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(s); err != nil {
		return err
	}
	buf.WriteString(strings.TrimSuffix(scratch.String(), "\n"))
	return nil
}
