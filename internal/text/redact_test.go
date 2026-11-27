package text

import "testing"

func TestRedact(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"leaves a plain line alone", "docker compose up --detach", "docker compose up --detach"},
		{"redacts an upper-case pair and keeps its name", "run API_KEY=hunter2 now", "run API_KEY=<redacted> now"},
		{"redacts a pair at the start of the line", "DB_URL=postgres://db/app", "DB_URL=<redacted>"},
		{"redacts every pair on a line", "A=1 B=2", "A=<redacted> B=<redacted>"},
		{"redacts a double-quoted value whole", `TOKEN="two words" next`, "TOKEN=<redacted> next"},
		{"redacts a single-quoted value whole", `TOKEN='two words' next`, "TOKEN=<redacted> next"},
		{"redacts a lower-case name that looks like a secret", "password=hunter2 Secret_Value=x AUTH_header=y", "password=<redacted> Secret_Value=<redacted> AUTH_header=<redacted>"},
		{"leaves a lower-case name that is no secret", "--filter label=com.docker.compose.project=x", "--filter label=com.docker.compose.project=x"},
		{"does not read a dotted name as a pair", "com.docker.compose.PROJECT=x", "com.docker.compose.PROJECT=x"},
		{"leaves an empty value", "FOO=", "FOO="},
		{"redacts the userinfo of a URL", "clone https://user:pa55@example.com/repo.git", "clone https://<redacted>@example.com/repo.git"},
		{"redacts a user with no password", "ssh://deploy@host/path", "ssh://<redacted>@host/path"},
		{"redacts a password with an at sign in it", "https://u:p@ss@host/x", "https://<redacted>@host/x"},
		{"leaves a URL with no userinfo", "http://login.localhost/a@b", "http://login.localhost/a@b"},
		{"does both in one line", "https://u:p@h/?TOKEN=abc", "https://<redacted>@h/?TOKEN=<redacted>"},
		{"redacts a double-quoted value that has a quote in it", `TOKEN="a \"b\" c" next`, "TOKEN=<redacted> next"},
		{"redacts an Authorization header to the end of the line", "curl -H Authorization: Basic abc123 now", "curl -H Authorization: <redacted>"},
		{"redacts a bearer token", "sent Bearer eyJhbGci.abc-def_1 ok", "sent Bearer <redacted> ok"},
		{"treats pwd, signature, private and jwt as secrets", "pwd=a Signature=b private_thing=c myjwt=d", "pwd=<redacted> Signature=<redacted> private_thing=<redacted> myjwt=<redacted>"},
		{"is idempotent", "A=<redacted> https://<redacted>@h", "A=<redacted> https://<redacted>@h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Redact(tc.in); got != tc.want {
				t.Fatalf("Redact(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestRedactArguments(t *testing.T) {
	for _, tc := range []struct {
		name     string
		in, want []string
	}{
		{"leaves treerig's own arguments", []string{"compose", "--project-name", "p", "up", "--detach"}, []string{"compose", "--project-name", "p", "up", "--detach"}},
		{"keeps a value with spaces whole", []string{"--format", "{{json .State}}", "--filter", "label=a b"}, []string{"--format", "{{json .State}}", "--filter", "label=a b"}},
		{"replaces the argument after a secret flag, however it is spelled", []string{"login", "--password", "p w 'x", "--token", "t"}, []string{"login", "--password", "<redacted>", "--token", "<redacted>"}},
		{"replaces the value of a pair after -e, --env and --build-arg", []string{"run", "-e", "name=has space", "--env", "B=2", "--build-arg", "C=3", "-e", "JUSTNAME"}, []string{"run", "-e", "name=<redacted>", "--env", "B=<redacted>", "--build-arg", "C=<redacted>", "-e", "JUSTNAME"}},
		{"replaces the value of a --flag=value with a secret name", []string{"--api-key=abc", "--db-password=x y", "--timeout=5"}, []string{"--api-key=<redacted>", "--db-password=<redacted>", "--timeout=5"}},
		{"replaces the value in --build-arg=K=V", []string{"--build-arg=K=V"}, []string{"--build-arg=K=<redacted>"}},
		{"keeps an ownership label readable, and hides one that looks secret", []string{"--label", "dev.treerig.worktree-uuid=abc", "--label", "TOKEN=x", "--label=api_secret=y"}, []string{"--label", "dev.treerig.worktree-uuid=abc", "--label", "TOKEN=<redacted>", "--label=api_secret=<redacted>"}},
		{"redacts a pair and a URL inside any argument", []string{"x", "API_TOKEN=abc", "https://u:p@h/"}, []string{"x", "API_TOKEN=<redacted>", "https://<redacted>@h/"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := RedactArguments(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %q, want %q", got, tc.want)
				}
			}
		})
	}
}
