package layout

import "testing"

func TestParsePortRange(t *testing.T) {
	t.Run("reads low-high", func(t *testing.T) {
		got, err := ParsePortRange("20000-29999")
		if err != nil || got != DefaultPortRange {
			t.Fatalf("ParsePortRange = %v, %v; want %v", got, err, DefaultPortRange)
		}
		if got.Size() != 10000 || !got.Contains(20000) || !got.Contains(29999) || got.Contains(30000) || got.Contains(19999) {
			t.Fatalf("Size/Contains disagree with %v", got)
		}
		if got.String() != "20000-29999" {
			t.Fatalf("String = %q", got.String())
		}
	})

	t.Run("accepts a single-port range", func(t *testing.T) {
		if got, err := ParsePortRange("30000-30000"); err != nil || got.Size() != 1 {
			t.Fatalf("ParsePortRange = %v, %v", got, err)
		}
	})

	for _, bad := range []string{"", "20000", "a-b", "20000-", "-20000", "1023-2000", "3000-2000", "30000-65536", "+3000-4000", "2000--3000", "2000-3000-4000"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			if _, err := ParsePortRange(bad); err == nil {
				t.Fatalf("ParsePortRange(%q) succeeded", bad)
			}
		})
	}
}
