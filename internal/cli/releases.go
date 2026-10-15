package cli

import (
	"net/http"
	"time"

	"github.com/sofired/berth/internal/releasefetch"
)

// newReleaseClient builds the HTTP client `teardown pin --release` and
// `--latest` download with (docs/design/teardown.md, "Download bounds").
// Proxy is set explicitly to http.ProxyFromEnvironment: net/http reads the
// proxy variables itself, a deliberate exception to "values come from getenv"
// that belongs here, in the entry point, and never in library code. A redirect
// to anything but https is refused. The transport's timeouts bound a stalled
// connection; the command's own context bounds the whole download.
func newReleaseClient() *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyFromEnvironment
	transport.TLSHandshakeTimeout = 30 * time.Second
	transport.ResponseHeaderTimeout = 60 * time.Second
	return &http.Client{Transport: transport, CheckRedirect: releasefetch.RequireHTTPS}
}
