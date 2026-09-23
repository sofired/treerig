//go:build tools

// Package tools pins modules the build does not import yet, so `go mod tidy`
// keeps them and their checksums while the spike is being assembled.
package tools

import (
	_ "github.com/BurntSushi/toml"
	_ "github.com/google/go-cmp/cmp"
	_ "github.com/rogpeppe/go-internal/testscript"
	_ "golang.org/x/sys/unix"
)
