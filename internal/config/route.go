package config

// The routing fields of a [service.<name>] table, checked on their own for a
// host route (docs/design/routing.md, decision 11): `route add` takes the
// same name, subdomain and path a service does, so it refuses a bad one in
// the schema's own words.

// CheckRouteName reports why name cannot name a service or a host route, or
// "" when it can: lower-case letters, digits and hyphens, at most 63
// characters.
func CheckRouteName(name string) string {
	switch {
	case !slugPattern.MatchString(name):
		return nameMessage
	case len(name) > dnsLabelMax:
		return labelLengthMessage
	}
	return ""
}

// CheckSubdomain reports why subdomain is not a valid route subdomain, or ""
// when it is. The empty subdomain is valid: it routes at the worktree host.
func CheckSubdomain(subdomain string) string {
	switch {
	case !subdomainPattern.MatchString(subdomain):
		return subdomainMessage
	case len(subdomain) > dnsLabelMax:
		return labelLengthMessage
	}
	return ""
}

// CheckRoutePath reports why path is not a valid route path prefix, or ""
// when it is.
func CheckRoutePath(path string) string {
	if !urlPathPattern.MatchString(path) {
		return pathMessage
	}
	return ""
}
