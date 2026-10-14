// Package teardowntest builds the smallest files that pass
// teardown.CheckExecutable, for tests that pin a "binary": an ELF or Mach-O
// header with no code in it. They are never run.
package teardowntest

import (
	"bytes"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"strings"
)

// Executable is a header-only executable for platform ("linux_amd64", …), with
// extra appended so tests can make distinct files for one platform. It panics
// on a platform berth does not build.
func Executable(platform string, extra string) []byte {
	goos, goarch, _ := strings.Cut(platform, "_")
	switch goos {
	case "linux":
		return append(ELF(map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[goarch], elf.ET_EXEC), extra...)
	case "darwin":
		return append(MachO(map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[goarch], macho.TypeExec), extra...)
	}
	panic("teardowntest: unknown platform " + platform)
}

// ELF is a 64-bit little-endian ELF header of the given machine and type, with
// no program or section headers.
func ELF(machine elf.Machine, typ elf.Type) []byte {
	var b bytes.Buffer
	b.Write([]byte{0x7f, 'E', 'L', 'F', byte(elf.ELFCLASS64), byte(elf.ELFDATA2LSB), byte(elf.EV_CURRENT), 0, 0, 0, 0, 0, 0, 0, 0, 0})
	put := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	put(uint16(typ))
	put(uint16(machine))
	put(uint32(1))  // version
	put(uint64(0))  // entry
	put(uint64(0))  // program header offset
	put(uint64(0))  // section header offset
	put(uint32(0))  // flags
	put(uint16(64)) // header size
	put(uint16(56)) // program header entry size
	put(uint16(0))  // program headers
	put(uint16(64)) // section header entry size
	put(uint16(0))  // section headers
	put(uint16(0))  // section name table
	return b.Bytes()
}

// MachO is a thin 64-bit little-endian Mach-O header of the given CPU and file
// type, with no load commands.
func MachO(cpu macho.Cpu, typ macho.Type) []byte {
	var b bytes.Buffer
	put := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	put(macho.Magic64)
	put(uint32(cpu))
	put(0) // subcpu
	put(uint32(typ))
	put(0) // load commands
	put(0) // size of load commands
	put(0) // flags
	put(0) // reserved
	return b.Bytes()
}

// Fat is a fat Mach-O file holding one thin file per entry of thin.
func Fat(thin ...[]byte) []byte {
	var head, body bytes.Buffer
	put := func(w *bytes.Buffer, v uint32) { _ = binary.Write(w, binary.BigEndian, v) }
	put(&head, macho.MagicFat)
	put(&head, uint32(len(thin)))
	offset := uint32(4096)
	for _, file := range thin {
		cpu := binary.LittleEndian.Uint32(file[4:8])
		put(&head, cpu)
		put(&head, 0) // subcpu
		put(&head, offset)
		put(&head, uint32(len(file)))
		put(&head, 12) // alignment, a power of two
		body.Write(file)
		offset += 4096
		body.Write(make([]byte, 4096-len(file)))
	}
	out := make([]byte, 4096)
	copy(out, head.Bytes())
	return append(out, body.Bytes()...)
}
