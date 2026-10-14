package config

import (
	"strings"
	"testing"
)

func TestCheckRouteFieldsUseTheSchemasOwnWords(t *testing.T) {
	t.Run("a route name is a service name", func(t *testing.T) {
		for _, name := range []string{"web", "api-v1", "a", "0day", strings.Repeat("a", 63)} {
			if why := CheckRouteName(name); why != "" {
				t.Errorf("%q refused: %s", name, why)
			}
		}
		for name, want := range map[string]string{
			"":                      nameMessage,
			"Web":                   nameMessage,
			"-web":                  nameMessage,
			"web_1":                 nameMessage,
			"a.b":                   nameMessage,
			strings.Repeat("a", 64): labelLengthMessage,
		} {
			if why := CheckRouteName(name); why != want {
				t.Errorf("%q: %q, want %q", name, why, want)
			}
		}
	})

	t.Run("a subdomain may be empty, and is one DNS label", func(t *testing.T) {
		for _, subdomain := range []string{"", "app", "a-b", strings.Repeat("a", 63)} {
			if why := CheckSubdomain(subdomain); why != "" {
				t.Errorf("%q refused: %s", subdomain, why)
			}
		}
		for subdomain, want := range map[string]string{
			"-a": subdomainMessage, "a-": subdomainMessage, "a.b": subdomainMessage, "A": subdomainMessage,
			strings.Repeat("a", 64): labelLengthMessage,
		} {
			if why := CheckSubdomain(subdomain); why != want {
				t.Errorf("%q: %q, want %q", subdomain, why, want)
			}
		}
	})

	t.Run("a path starts with a slash and carries nothing a rule or URL would misread", func(t *testing.T) {
		for _, path := range []string{"/", "/v1", "/a/b-c_d.e~f", "/%2F", "/a;b"} {
			if why := CheckRoutePath(path); why != "" {
				t.Errorf("%q refused: %s", path, why)
			}
		}
		for _, path := range []string{"", "v1", "/a b", "/a`b", "/a$b", "/a?b", "/a#b", "/%zz", `/a"b`} {
			if why := CheckRoutePath(path); why != pathMessage {
				t.Errorf("%q: %q, want %q", path, why, pathMessage)
			}
		}
	})
}
