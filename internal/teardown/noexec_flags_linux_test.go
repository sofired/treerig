package teardown

import "golang.org/x/sys/unix"

// noExecFlagsType is the type of statfs's flags on this platform.
type noExecFlagsType = int64

// noExecConstant is this platform's noexec mount flag.
const noExecConstant = unix.ST_NOEXEC
