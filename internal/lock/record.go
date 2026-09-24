package lock

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/sofired/berth/internal/procid"
)

// recordMaxBytes: a record is one short line; anything larger is not ours and is not read.
const recordMaxBytes = 4_096

// FileID is the identity of the lock target file a record was written for —
// a record describing a different (dev, ino) belongs to an earlier
// incarnation of the target path.
type FileID struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// Record is what the holder wrote about itself into the sidecar file when it
// acquired the lock. Read across versions of this tool, so unknown fields
// are ignored, not refused — the same contract src/operation-lock.ts's zod
// schema gave holderRecordSchema.
type Record struct {
	PID        int        `json:"pid"`
	Host       string     `json:"host"`
	Command    string     `json:"command"`
	StartedAt  time.Time  `json:"startedAt"`
	ReleasedAt *time.Time `json:"releasedAt,omitempty"`
	File       FileID     `json:"file"`
	MachineID  string     `json:"machineId,omitempty"`
	BootID     string     `json:"bootId,omitempty"`
	PIDNS      string     `json:"pidNs,omitempty"`
	StartTicks *uint64    `json:"startTicks,omitempty"`
	StartUs    *int64     `json:"startUs,omitempty"`
}

func (r Record) valid() bool {
	return r.PID > 0 && r.Host != ""
}

// readRecord reads and validates the record at path, returning (nil, nil,
// nil) when it is missing, empty, oversized, or not a record at all —
// exactly as unreadable as a record nobody wrote.
func readRecord(path string) (*Record, error) {
	info, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if info.Size() == 0 || info.Size() > recordMaxBytes {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec Record
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, nil
	}
	if !rec.valid() {
		return nil, nil
	}
	return &rec, nil
}

// writeRecordAtomic writes rec to path via a temp file and rename, so a
// reader never observes a partial record.
func writeRecordAtomic(path string, rec Record) error {
	data, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".holder-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	success = true
	return nil
}

// VerdictKind is what judgeHolder concluded about a previous holder.
type VerdictKind int

const (
	// Gone means the holder is proven unable to still be running.
	Gone VerdictKind = iota
	// Alive means it may still be running; nothing overrides this.
	Alive
	// Unverifiable means this machine cannot check it; only a person can.
	Unverifiable
)

// Verdict is what the lock's record, this machine, and the process table say about a holder.
type Verdict struct {
	Kind   VerdictKind
	Reason string
}

// startTolerance bounds how far apart a macOS holder's recorded start and a
// fresh Inspect's may be before a PID is judged reused, the way
// src/lock-recovery.ts's START_TOLERANCE_MS bounded ps's one-second steps.
const startToleranceUs = 2_000_000

// judgeHolder is a port of judgeHolder from src/lock-recovery.ts: same
// machine or boot, same PID namespace, PID absent, zombie or reused.
// skipFileIdentity is set only for a TypeScript-leftover record, whose
// "earlier lock" concept (a `.lock` directory recreated under the same
// target) does not exist in the flock design; the leftover's mere presence
// already proves which target it describes.
func judgeHolder(record *Record, currentFile FileID, self procid.Identity, hostname string, table procid.Table, skipFileIdentity bool) (Verdict, error) {
	if record == nil {
		return Verdict{Kind: Unverifiable, Reason: "its holder left no record of who it is"}, nil
	}
	who := fmt.Sprintf("PID %d on %s", record.PID, quoteOneLine(record.Host))

	if !skipFileIdentity && record.File != currentFile {
		return Verdict{Kind: Unverifiable, Reason: fmt.Sprintf(
			"its record, naming %s, belongs to an earlier lock, so its holder never identified itself", who)}, nil
	}

	if record.MachineID == "" && record.BootID == "" {
		// Written by an earlier version, or by a holder that could not read its
		// machine's identity: only the hostname to go on.
		if record.Host != hostname {
			return Verdict{Kind: Unverifiable, Reason: fmt.Sprintf("its holder, %s, ran on another host", who)}, nil
		}
		status, err := table.Inspect(record.PID)
		if err != nil {
			return Verdict{}, err
		}
		if status.State == procid.Absent || status.State == procid.Zombie {
			return Verdict{Kind: Unverifiable, Reason: fmt.Sprintf(
				"%s is gone, but its record names no machine or boot, so whether that PID was this machine's cannot be told", who)}, nil
		}
		return Verdict{Kind: Alive, Reason: fmt.Sprintf(
			"%s is running, and its record names no machine or boot, so whether that is the holder cannot be told", who)}, nil
	}

	// This machine, by its machine ID where both sides have one. Where either
	// has none (a container image without /etc/machine-id, say), by the boot:
	// only the same running kernel has the same boot ID, and the PID namespace
	// below tells its containers apart.
	if record.MachineID != "" && self.MachineID != "" {
		if record.MachineID != self.MachineID {
			return Verdict{Kind: Unverifiable, Reason: fmt.Sprintf("its holder, %s, ran on another machine", who)}, nil
		}
		if record.BootID != "" && self.BootID != "" && record.BootID != self.BootID {
			return Verdict{Kind: Gone, Reason: fmt.Sprintf("this machine has restarted since %s took it", who)}, nil
		}
	} else if record.BootID == "" || self.BootID == "" {
		return Verdict{Kind: Unverifiable, Reason: fmt.Sprintf("this machine's identity cannot be read, so %s cannot be checked", who)}, nil
	} else if record.BootID != self.BootID {
		return Verdict{Kind: Unverifiable, Reason: fmt.Sprintf(
			"its holder, %s, ran on another machine or before this one restarted, and with no machine ID to go on, this command cannot tell which", who)}, nil
	}

	effectivePIDNS := record.PIDNS
	if effectivePIDNS == "" {
		effectivePIDNS = self.PIDNS
	}
	if effectivePIDNS != "" && record.PIDNS != self.PIDNS {
		return Verdict{Kind: Unverifiable, Reason: fmt.Sprintf(
			"its holder, %s, ran in another PID namespace (a container, say), which this command cannot see into", who)}, nil
	}

	status, err := table.Inspect(record.PID)
	if err != nil {
		return Verdict{}, err
	}
	if status.State == procid.Absent {
		return Verdict{Kind: Gone, Reason: fmt.Sprintf("%s no longer exists", who)}, nil
	}
	if status.State == procid.Zombie {
		return Verdict{Kind: Gone, Reason: fmt.Sprintf("%s has exited", who)}, nil
	}

	// A PID is reused. The process there now is the holder only if it started when the holder did.
	reused := false
	switch {
	case record.StartTicks != nil && status.StartTicks != nil:
		reused = *status.StartTicks != *record.StartTicks
	case record.StartUs != nil && status.StartUs != nil:
		diff := *status.StartUs - *record.StartUs
		if diff < 0 {
			diff = -diff
		}
		reused = diff > startToleranceUs
	}
	if reused {
		return Verdict{Kind: Gone, Reason: fmt.Sprintf("%s has exited; that PID now belongs to a later process", who)}, nil
	}
	if status.State == procid.Stopped {
		return Verdict{Kind: Alive, Reason: fmt.Sprintf(
			"%s is stopped, not gone: resume it with `kill -CONT %d` and let it finish, or end it", who, record.PID)}, nil
	}
	return Verdict{Kind: Alive, Reason: fmt.Sprintf("%s is still running", who)}, nil
}
