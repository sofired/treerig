package config

import (
	"slices"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/sofired/berth/internal/shape"
)

// DecodeOrderedTOML parses contents and rebuilds it as a [shape.Obj] tree
// that remembers the order its keys appeared in the file. shape's
// "Unrecognized key(s)" message, and its schema-order defaulting, both
// depend on that order — BurntSushi/toml decodes into an ordinary
// map[string]any, which (like every Go map) does not preserve it, so this
// pairs the decoded value back up with the key order from its MetaData.
//
// Exported so internal/settings can decode settings.toml the same strict
// way berth.toml is decoded below (Parse), without duplicating this bridge
// between BurntSushi's decoder and shape's own ordered object.
func DecodeOrderedTOML(contents string) (*shape.Obj, error) {
	var generic map[string]any
	meta, err := toml.Decode(contents, &generic)
	if err != nil {
		return nil, err
	}
	order := keyOrder(meta.Keys(), generic)
	return build(order, "", generic).(*shape.Obj), nil
}

// keyOrder records, for every table in the document, the order its own keys
// were written in. Tables are named by their concrete path — segments joined
// by NUL, with an array-of-tables element named "#<index>" — because TOML
// lets one [[x]] block's fields and the next be separated by any other table,
// so no single cursor through the keys can attribute them. MetaData.Keys()
// lists every key in file order, and an array-of-tables' header once per
// occurrence, which is how the occurrence a key belongs to is known.
func keyOrder(keys []toml.Key, root map[string]any) map[string][]string {
	order := map[string][]string{}
	seen := map[string]bool{}
	occurrence := map[string]int{}
	record := func(parent, name string) {
		if id := parent + "\x00" + name; !seen[id] {
			seen[id] = true
			order[parent] = append(order[parent], name)
		}
	}
	for _, key := range keys {
		path := ""
		var node any = root
		for i, name := range key {
			record(path, name)
			path = join(path, name)
			table, _ := node.(map[string]any)
			child := table[name]
			array, isArray := child.([]map[string]any)
			if !isArray {
				node = child
				continue
			}
			if i == len(key)-1 {
				// A [[x]] header: the next occurrence starts here.
				occurrence[path]++
			}
			index := max(occurrence[path]-1, 0)
			if index >= len(array) {
				node = nil
				break
			}
			path = join(path, "#"+strconv.Itoa(index))
			node = array[index]
		}
	}
	return order
}

// build turns a decoded value into shape's ordered form: tables become Objs
// in their recorded order, arrays of tables become []any of Objs.
func build(order map[string][]string, path string, value any) any {
	switch v := value.(type) {
	case map[string]any:
		obj := shape.NewObj()
		for _, name := range orderedNames(order[path], v) {
			obj.Set(name, build(order, join(path, name), v[name]))
		}
		return obj
	case []map[string]any:
		items := make([]any, len(v))
		for i, element := range v {
			items[i] = build(order, join(path, "#"+strconv.Itoa(i)), element)
		}
		return items
	case []any:
		items := make([]any, len(v))
		for i, element := range v {
			items[i] = build(order, join(path, "#"+strconv.Itoa(i)), element)
		}
		return items
	default:
		return v
	}
}

// orderedNames is every key of m: first as recorded, then — for anything the
// metadata did not list, as inside an inline table — in sorted order, so the
// result never depends on map iteration.
func orderedNames(recorded []string, m map[string]any) []string {
	names := make([]string, 0, len(m))
	for _, name := range recorded {
		if _, ok := m[name]; ok {
			names = append(names, name)
		}
	}
	var rest []string
	for name := range m {
		if !slices.Contains(names, name) {
			rest = append(rest, name)
		}
	}
	slices.Sort(rest)
	return append(names, rest...)
}

// join appends name to a table path, using NUL as the separator since it
// cannot appear in a TOML key.
func join(path, name string) string {
	if path == "" {
		return name
	}
	return strings.Join([]string{path, name}, "\x00")
}
