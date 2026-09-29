package shape

import "testing"

func TestBool(t *testing.T) {
	t.Run("accepts true and false as given", func(t *testing.T) {
		for _, want := range []bool{true, false} {
			value, issues := Parse(Bool(), want)
			if len(issues) != 0 || value != want {
				t.Fatalf("Parse(%v) = %v, %#v", want, value, issues)
			}
		}
	})

	t.Run("rejects a string, naming what it received", func(t *testing.T) {
		_, issues := Parse(Bool(), "true")
		issue := mustIssue(t, issues, "(root)", CodeInvalidType)
		if want := "Invalid input: expected boolean, received string"; issue.Message != want {
			t.Fatalf("message = %q, want %q", issue.Message, want)
		}
	})

	t.Run("an absent field takes its default, or is reported when required", func(t *testing.T) {
		schema := Object(F("on", Bool().Default(true)), F("need", Bool())).Strict()
		value, issues := Parse(schema, NewObj())
		if len(issues) != 1 || issues[0].Message != "Invalid input: expected boolean, received undefined" {
			t.Fatalf("issues = %#v", issues)
		}
		if got, _ := value.(*Obj).Get("on"); got != true {
			t.Fatalf("on = %v, want the default true", got)
		}
	})

	t.Run("a union names boolean among the types it expected", func(t *testing.T) {
		_, issues := Parse(Union(Bool(), String()), 3)
		if len(issues) != 1 || issues[0].Message != "Invalid input: expected boolean or string, received number" {
			t.Fatalf("issues = %#v", issues)
		}
	})
}
