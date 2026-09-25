package procid

import (
	"reflect"
	"testing"
)

// statLine builds /proc/<pid>/stat with the given command name, state and
// start ticks — the fixture tests/unit/process-table.test.ts uses.
func statLine(comm, state string, startTicks uint64) string {
	return "42 (" + comm + ") " + state +
		" 1 42 42 0 -1 4194304 0 0 0 0 0 0 0 0 20 0 1 0 " +
		itoa(startTicks) + " 5971968 416 18446744073709551615\n"
}

// itoa formats n in base 10, without pulling in strconv just for the fixture.
func itoa(n uint64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

func TestParsing(t *testing.T) {
	t.Run("reads state and start ticks after the last parenthesis, whatever the command name holds", func(t *testing.T) {
		state, ticks, ok := parseProcStat(statLine("cat", "R", 212235381))
		if !ok || state != "R" || ticks != 212235381 {
			t.Fatalf("got %q %d %v", state, ticks, ok)
		}
		state, ticks, ok = parseProcStat(statLine("evil) Z 1 (x", "S", 7))
		if !ok || state != "S" || ticks != 7 {
			t.Fatalf("got %q %d %v", state, ticks, ok)
		}
		if _, _, ok := parseProcStat("no parenthesis at all"); ok {
			t.Fatal("expected no match")
		}
		if _, _, ok := parseProcStat("1 (short) R 1 2"); ok {
			t.Fatal("expected no match")
		}
	})

	t.Run("names process states the way both /proc and ps spell them", func(t *testing.T) {
		for _, letter := range []byte{'Z', 'X', 'x'} {
			if got := stateOf(letter); got != Zombie {
				t.Errorf("stateOf(%q) = %v, want Zombie", letter, got)
			}
		}
		for _, letter := range []byte{'T', 't'} {
			if got := stateOf(letter); got != Stopped {
				t.Errorf("stateOf(%q) = %v, want Stopped", letter, got)
			}
		}
		for _, letter := range []byte{'R', 'S', 'D', 'I'} {
			if got := stateOf(letter); got != Running {
				t.Errorf("stateOf(%q) = %v, want Running", letter, got)
			}
		}
	})

	t.Run("reads ps elapsed times of every length, and nothing else", func(t *testing.T) {
		cases := []struct {
			in   string
			want int64
			ok   bool
		}{
			{"05", 0, false},
			{"01:05", 65, true},
			{"02:01:05", 7_265, true},
			{"3-02:01:05", 266_465, true},
			{" 00:07 ", 7, true},
			{"soon", 0, false},
		}
		for _, tc := range cases {
			got, ok := parseEtime(tc.in)
			if ok != tc.ok || (ok && got != tc.want) {
				t.Errorf("parseEtime(%q) = %d, %v; want %d, %v", tc.in, got, ok, tc.want, tc.ok)
			}
		}
	})

	t.Run("trusts a ps line only for the PID it asked about", func(t *testing.T) {
		stat, elapsed, ok := parsePsLine("  4242 Ss   01:02:03", 4242)
		if !ok || stat != "Ss" || elapsed != 3_723 {
			t.Fatalf("got %q %d %v", stat, elapsed, ok)
		}
		// BusyBox ignores -p and prints the whole table: the first line is PID 1.
		if _, _, ok := parsePsLine("    1 S    99-00:00:00", 4242); ok {
			t.Fatal("expected no match")
		}
		if _, _, ok := parsePsLine("4242 ?? garbage", 4242); ok {
			t.Fatal("expected no match")
		}
	})

	t.Run("reads the platform UUID out of ioreg", func(t *testing.T) {
		uuid, ok := parseIoregUUID("  \"IOPlatformUUID\" = \"564D1D23-ABCD-4E5F-8A9B-0123456789AB\"\n")
		if !ok || uuid != "564D1D23-ABCD-4E5F-8A9B-0123456789AB" {
			t.Fatalf("got %q %v", uuid, ok)
		}
		if _, ok := parseIoregUUID("nothing here"); ok {
			t.Fatal("expected no match")
		}
	})

	t.Run("offers each whole token of a ps scan as both an argument and an environment entry, skipping the excluded", func(t *testing.T) {
		text := "  10 docker compose --project-name berth-abc up BERTH_WORKTREE_ID=w\n  11 ps -A\n junk\n"
		scanned := parsePsScan(text, map[int]bool{11: true})
		want := []Proc{{
			PID:     10,
			Args:    []string{"docker", "compose", "--project-name", "berth-abc", "up", "BERTH_WORKTREE_ID=w"},
			Env:     []string{"docker", "compose", "--project-name", "berth-abc", "up", "BERTH_WORKTREE_ID=w"},
			EnvRead: true,
		}}
		if !reflect.DeepEqual(scanned, want) {
			t.Fatalf("got %+v, want %+v", scanned, want)
		}
	})
}

func TestParseProcArgs2(t *testing.T) {
	t.Run("splits argc, the exec path, argv and the environment out of the kern.procargs2 buffer", func(t *testing.T) {
		var raw []byte
		putU32 := func(n uint32) {
			raw = append(raw, byte(n), byte(n>>8), byte(n>>16), byte(n>>24))
		}
		putU32(2) // argc
		raw = append(raw, "/usr/bin/sleep\x00\x00\x00\x00"...)
		raw = append(raw, "sleep\x00"...)
		raw = append(raw, "30\x00"...)
		raw = append(raw, "PATH=/bin\x00"...)
		raw = append(raw, "HOME=/root\x00"...)

		argv, envp, ok := parseProcArgs2(raw)
		if !ok {
			t.Fatal("expected ok")
		}
		if !reflect.DeepEqual(argv, []string{"sleep", "30"}) {
			t.Fatalf("argv = %v", argv)
		}
		if !reflect.DeepEqual(envp, []string{"PATH=/bin", "HOME=/root"}) {
			t.Fatalf("envp = %v", envp)
		}
	})

	t.Run("reads what is there when argc claims more arguments than the buffer holds", func(t *testing.T) {
		raw := []byte{0xff, 0xff, 0xff, 0x7f} // argc = 2^31-1
		raw = append(raw, "/bin/sh\x00\x00sh\x00"...)
		argv, envp, ok := parseProcArgs2(raw)
		if !ok || !reflect.DeepEqual(argv, []string{"sh"}) || len(envp) != 0 {
			t.Fatalf("argv = %q, envp = %q, ok = %v", argv, envp, ok)
		}
	})

	t.Run("refuses a buffer too short to hold even argc", func(t *testing.T) {
		if _, _, ok := parseProcArgs2([]byte{1, 2}); ok {
			t.Fatal("expected no match")
		}
	})
}
