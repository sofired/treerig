//go:build !linux

package teardown

import "golang.org/x/sys/unix"

// noExecFlagsType is the type of statfs's flags on this platform.
type noExecFlagsType = uint32

// noExecConstant is this platform's noexec mount flag.
const noExecConstant = unix.MNT_NOEXEC
