package teardown

import "testing"

func TestNoExecFlag(t *testing.T) {
	if !noExecFlag(noExecFlagsType(noExecConstant)) {
		t.Error("the platform's noexec flag was not recognised")
	}
	if !noExecFlag(noExecFlagsType(noExecConstant) | 1) {
		t.Error("the noexec flag among others was not recognised")
	}
	if noExecFlag(0) || noExecFlag(1) {
		t.Error("mount flags without noexec were taken for it")
	}
}
