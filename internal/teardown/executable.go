package teardown

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"errors"
	"fmt"
	"strings"
)

// CheckExecutable reports why data is not an executable for platform (a
// "<GOOS>_<GOARCH>" key), or nil. It reads only the file's headers, with the
// standard library's debug/elf for linux and debug/macho for darwin, and runs
// nothing. It refuses an empty file, a file of another format, and one built for
// another architecture; a fat Mach-O passes when it holds the architecture.
//
// It is the check `teardown pin --file` makes before it asks, so that a digest
// that matches the wrong file (an empty file, a binary for another platform)
// cannot become a pin that `teardown status` reports as intact. The error text
// is the reason only, such as "not an ELF file", for the command to wrap.
func CheckExecutable(data []byte, platform string) error {
	goos, goarch, _ := strings.Cut(platform, "_")
	if len(data) == 0 {
		return errors.New("the file is empty")
	}
	switch goos {
	case "linux":
		return checkELF(data, goarch)
	case "darwin":
		return checkMachO(data, goarch)
	}
	return fmt.Errorf("%s is not a platform treerig builds", platform)
}

// checkELF requires an executable ELF file for goarch.
func checkELF(data []byte, goarch string) error {
	file, err := elf.NewFile(bytes.NewReader(data))
	if err != nil {
		return errors.New("not an ELF file")
	}
	defer func() { _ = file.Close() }()
	want, ok := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[goarch]
	if !ok {
		return fmt.Errorf("%s is not an architecture treerig builds", goarch)
	}
	if file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN {
		return fmt.Errorf("an ELF file of type %s, not an executable", file.Type)
	}
	if file.Machine != want {
		return fmt.Errorf("an ELF file for %s, not %s", file.Machine, want)
	}
	return nil
}

// checkMachO requires an executable Mach-O file for goarch, thin or fat.
func checkMachO(data []byte, goarch string) error {
	want, ok := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[goarch]
	if !ok {
		return fmt.Errorf("%s is not an architecture treerig builds", goarch)
	}
	if fat, err := macho.NewFatFile(bytes.NewReader(data)); err == nil {
		defer func() { _ = fat.Close() }()
		for _, arch := range fat.Arches {
			if arch.Cpu == want && arch.Type == macho.TypeExec {
				return nil
			}
		}
		return fmt.Errorf("a fat Mach-O file with no executable for %s", want)
	} else if !errors.Is(err, macho.ErrNotFat) {
		return errors.New("not a Mach-O file")
	}
	file, err := macho.NewFile(bytes.NewReader(data))
	if err != nil {
		return errors.New("not a Mach-O file")
	}
	defer func() { _ = file.Close() }()
	if file.Type != macho.TypeExec {
		return fmt.Errorf("a Mach-O file of type %s, not an executable", file.Type)
	}
	if file.Cpu != want {
		return fmt.Errorf("a Mach-O file for %s, not %s", file.Cpu, want)
	}
	return nil
}
