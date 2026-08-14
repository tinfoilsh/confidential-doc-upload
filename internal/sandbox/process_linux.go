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
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{}); err != nil {
		return fmt.Errorf("disable core dumps: %w", err)
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
		resource int
		value    uint64
		name     string
	}{
		{unix.RLIMIT_AS, limits.AddressSpaceBytes, "address space"},
		{unix.RLIMIT_CPU, limits.CPUSeconds, "CPU"},
		{unix.RLIMIT_NOFILE, limits.OpenFiles, "open files"},
		{unix.RLIMIT_FSIZE, 0, "file size"},
		{unix.RLIMIT_CORE, 0, "core dump"},
		{unix.RLIMIT_MEMLOCK, 0, "locked memory"},
		{unix.RLIMIT_MSGQUEUE, 0, "message queue"},
	}
	for _, setting := range settings {
		if setting.value == 0 && setting.resource != unix.RLIMIT_FSIZE && setting.resource != unix.RLIMIT_CORE && setting.resource != unix.RLIMIT_MEMLOCK && setting.resource != unix.RLIMIT_MSGQUEUE {
			return fmt.Errorf("%s limit must be positive", setting.name)
		}
		limit := unix.Rlimit{Cur: setting.value, Max: setting.value}
		if err := unix.Setrlimit(setting.resource, &limit); err != nil {
			return fmt.Errorf("set %s limit: %w", setting.name, err)
		}
	}
	return nil
}
