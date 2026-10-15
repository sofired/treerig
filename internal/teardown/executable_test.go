package teardown_test

import (
	"debug/elf"
	"debug/macho"
	"strings"
	"testing"

	"github.com/sofired/berth/internal/teardown"
	"github.com/sofired/berth/internal/teardown/teardowntest"
)

func TestCheckExecutable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     []byte
		platform string
		want     string // "" for accepted, else the reason
	}{
		{"an ELF executable for linux_amd64", teardowntest.ELF(elf.EM_X86_64, elf.ET_EXEC), "linux_amd64", ""},
		{"a position-independent ELF for linux_arm64", teardowntest.ELF(elf.EM_AARCH64, elf.ET_DYN), "linux_arm64", ""},
		{"a thin Mach-O for darwin_arm64", teardowntest.MachO(macho.CpuArm64, macho.TypeExec), "darwin_arm64", ""},
		{"a fat Mach-O holding darwin_amd64", teardowntest.Fat(teardowntest.MachO(macho.CpuArm64, macho.TypeExec), teardowntest.MachO(macho.CpuAmd64, macho.TypeExec)), "darwin_amd64", ""},
		{"an empty file", nil, "linux_amd64", "the file is empty"},
		{"text", []byte("#!/bin/sh\necho hi\n"), "linux_amd64", "not an ELF file"},
		{"text on darwin", []byte("#!/bin/sh\necho hi\n"), "darwin_arm64", "not a Mach-O file"},
		{"an ELF for the other architecture", teardowntest.ELF(elf.EM_AARCH64, elf.ET_EXEC), "linux_amd64", "an ELF file for EM_AARCH64, not EM_X86_64"},
		{"an ELF object file", teardowntest.ELF(elf.EM_X86_64, elf.ET_REL), "linux_amd64", "an ELF file of type ET_REL, not an executable"},
		{"a Mach-O on linux", teardowntest.MachO(macho.CpuAmd64, macho.TypeExec), "linux_amd64", "not an ELF file"},
		{"an ELF on darwin", teardowntest.ELF(elf.EM_X86_64, elf.ET_EXEC), "darwin_amd64", "not a Mach-O file"},
		{"a thin Mach-O for the other architecture", teardowntest.MachO(macho.CpuAmd64, macho.TypeExec), "darwin_arm64", "a Mach-O file for CpuAmd64, not CpuArm64"},
		{"a Mach-O that is not an executable", teardowntest.MachO(macho.CpuArm64, macho.TypeDylib), "darwin_arm64", "a Mach-O file of type Dylib, not an executable"},
		{"a fat Mach-O without the architecture", teardowntest.Fat(teardowntest.MachO(macho.CpuArm64, macho.TypeExec)), "darwin_amd64", "a fat Mach-O file with no executable for CpuAmd64"},
		{"a truncated ELF", teardowntest.ELF(elf.EM_X86_64, elf.ET_EXEC)[:20], "linux_amd64", "not an ELF file"},
		{"an unknown platform", teardowntest.ELF(elf.EM_X86_64, elf.ET_EXEC), "freebsd_amd64", "freebsd_amd64 is not a platform berth builds"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := teardown.CheckExecutable(tc.data, tc.platform)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.want != "" && err == nil:
				t.Fatalf("accepted, want %q", tc.want)
			case tc.want != "" && !strings.Contains(err.Error(), tc.want):
				t.Fatalf("error = %q, want %q", err, tc.want)
			}
		})
	}

	t.Run("Executable builds a file CheckExecutable accepts for every platform", func(t *testing.T) {
		for _, platform := range teardown.ReleasePlatforms {
			if err := teardown.CheckExecutable(teardowntest.Executable(platform, "x"), platform); err != nil {
				t.Errorf("%s: %v", platform, err)
			}
		}
	})
}
