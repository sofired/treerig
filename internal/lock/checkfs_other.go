//go:build !linux && !darwin

package lock

// unsupportedFSType never refuses on a platform this tool does not support
// otherwise; there is no Windows build, and CheckFS's self-test still runs.
func unsupportedFSType(string) (string, bool) { return "", false }
