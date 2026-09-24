package shape

import (
	"bytes"
	"encoding/json"
)

// Obj is an ordered object: a decoded value together with the order its keys
// appeared in the source. zod's own parsed output, and the order it lists
// unrecognized keys in, follow the source's order — Go's map type does not,
// so a caller decoding TOML, JSON or anything else into this package builds
// one of these rather than a plain map[string]any.
//
// [Object.parse] also returns one on success: its Keys are the schema's own
// field order, which is what a caller re-serializing the result (rather than
// decoding it into a struct of its own) needs.
type Obj struct {
	keys   []string
	values map[string]any
}

// NewObj returns an empty ordered object, ready for Set.
func NewObj() *Obj {
	return &Obj{values: map[string]any{}}
}

// Set adds or overwrites the value at key, appending key to Keys the first
// time it is set. Returns o, so a source decoder can chain calls.
func (o *Obj) Set(key string, value any) *Obj {
	if _, exists := o.values[key]; !exists {
		o.keys = append(o.keys, key)
	}
	o.values[key] = value
	return o
}

// Get reports the value at key and whether it was set.
func (o *Obj) Get(key string) (any, bool) {
	v, ok := o.values[key]
	return v, ok
}

// Keys are every key that was Set, in the order it was first set.
func (o *Obj) Keys() []string {
	return append([]string(nil), o.keys...)
}

// Len is the number of keys set.
func (o *Obj) Len() int { return len(o.keys) }

// MarshalJSON writes the object in Keys order, so a value [Parse] produced
// can be serialized directly — without decoding it into a struct first — and
// still match the key order zod's own JSON.stringify would use.
func (o *Obj) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		b.Write(kb)
		b.WriteByte(':')
		vb, err := json.Marshal(o.values[k])
		if err != nil {
			return nil, err
		}
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}
