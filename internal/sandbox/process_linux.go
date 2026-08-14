package sandbox

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ProtectProcess prevents same-UID parser processes from inspecting this
// process through procfs or ptrace. The container deliberately runs without
// CAP_SYS_PTRACE, so a non-dumpable router/parser broker is not readable by a
// compromised child.
func ProtectProcess() error {
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		return fmt.Errorf("disable process dumpability: %w", err)
	}
	return nil
}

type Limits struct {
	AddressSpaceBytes uint64
	CPUSeconds        uint64
	OpenFiles         uint64
}

// ApplyLimits installs hard kernel resource limits inherited across exec.
func ApplyLimits(limits Limits) error {
	settings := []struct {
		resource  int
		value     uint64
		name      string
		allowZero bool
	}{
		{resource: unix.RLIMIT_AS, value: limits.AddressSpaceBytes, name: "address space"},
		{resource: unix.RLIMIT_CPU, value: limits.CPUSeconds, name: "CPU"},
		{resource: unix.RLIMIT_NOFILE, value: limits.OpenFiles, name: "open files"},
		{resource: unix.RLIMIT_FSIZE, name: "file size", allowZero: true},
		{resource: unix.RLIMIT_MEMLOCK, name: "locked memory", allowZero: true},
		{resource: unix.RLIMIT_MSGQUEUE, name: "message queue", allowZero: true},
	}
	for _, setting := range settings {
		if setting.value == 0 && !setting.allowZero {
			return fmt.Errorf("%s limit must be positive", setting.name)
		}
		limit := unix.Rlimit{Cur: setting.value, Max: setting.value}
		if err := unix.Setrlimit(setting.resource, &limit); err != nil {
			return fmt.Errorf("set %s limit: %w", setting.name, err)
		}
	}
	return nil
}
