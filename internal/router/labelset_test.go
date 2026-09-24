package router

import "testing"

// TestLabelSetQuotesLikeJSONStringify pins labelSet's quoting to
// JSON.stringify's: `<`, `>`, `&` and U+2028/U+2029 stay raw, where
// encoding/json would escape them, and insertion order is kept. MarshalJSON
// is called directly: json.Marshal would re-escape its output.
func TestLabelSetQuotesLikeJSONStringify(t *testing.T) {
	set := labels(kv("z<key>", "a&b"), kv("a", "line para "), kv("quote", `"\`))
	got, err := set.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"z<key>":"a&b","a":"line` + " " + `para` + " " + `","quote":"\"\\"}`
	if string(got) != want {
		t.Errorf("labelSet JSON:\n got %s\nwant %s", got, want)
	}
}
