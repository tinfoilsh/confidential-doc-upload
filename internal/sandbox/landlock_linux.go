package sandbox

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	pdfParserExecutable = "/usr/local/bin/pdfparser"
	pythonExecutable    = "/usr/local/bin/python3"
)

// parserLoaderPath is the only runtime file the statically linked MuPDF
// parser needs. Python receives its immutable libraries and source in the
// operation-specific branch below. In particular, /proc, /run, /tmp, /sys,
// /etc, and the other parser executable are absent. Input and output use
// inherited pipes and need no paths.
var parserLoaderPath = filesystemRule{path: "/lib/ld-musl-x86_64.so.1", executable: true}

type filesystemRule struct {
	path       string
	executable bool
}

const landlockAccessFSV1 = unix.LANDLOCK_ACCESS_FS_EXECUTE |
	unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_FILE |
	unix.LANDLOCK_ACCESS_FS_READ_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
	unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
	unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
	unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
	unix.LANDLOCK_ACCESS_FS_MAKE_REG |
	unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
	unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
	unix.LANDLOCK_ACCESS_FS_MAKE_SYM

// RestrictParserFilesystem creates an irreversible, allowlisted Landlock
// domain for one parser process. It deliberately fails closed if Landlock is
// unavailable: cvmimage v0.11.0 enables Landlock, and silently losing this
// boundary would make concurrent same-UID parsers unsafe.
func RestrictParserFilesystem(executable string) error {
	rules, err := parserRuntimePaths(executable)
	if err != nil {
		return err
	}
	return restrictFilesystem(rules)
}

func parserRuntimePaths(executable string) ([]filesystemRule, error) {
	switch executable {
	case pdfParserExecutable:
		return []filesystemRule{
			parserLoaderPath,
			{path: pdfParserExecutable, executable: true},
		}, nil
	case pythonExecutable:
		return []filesystemRule{
			parserLoaderPath,
			{path: pythonExecutable, executable: true},
			{path: "/usr/local/lib"},
			{path: "/usr/lib"},
			{path: "/lib"},
			{path: "/usr/share"},
			{path: "/app/docparser.py"},
		}, nil
	default:
		return nil, fmt.Errorf("unsupported parser executable %q", executable)
	}
}

func restrictFilesystem(rules []filesystemRule) error {
	abi, err := landlockABI()
	if err != nil {
		return err
	}

	handled := uint64(landlockAccessFSV1)
	if abi >= 2 {
		handled |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		handled |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	if abi >= 5 {
		handled |= unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
	}

	attributes := unix.LandlockRulesetAttr{Access_fs: handled}
	ruleset, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&attributes)),
		unsafe.Sizeof(attributes.Access_fs),
		0,
	)
	if errno != 0 {
		return fmt.Errorf("create Landlock ruleset: %w", errno)
	}
	defer unix.Close(int(ruleset))

	for _, rule := range rules {
		allowed := uint64(unix.LANDLOCK_ACCESS_FS_READ_FILE | unix.LANDLOCK_ACCESS_FS_READ_DIR)
		if rule.executable {
			allowed |= unix.LANDLOCK_ACCESS_FS_EXECUTE
		}
		if err := addLandlockPath(int(ruleset), rule.path, allowed); err != nil {
			return err
		}
	}

	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges for Landlock: %w", err)
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, ruleset, 0, 0)
	if errno != 0 {
		return fmt.Errorf("enter Landlock domain: %w", errno)
	}
	return nil
}

func landlockABI() (int, error) {
	version, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		0,
		0,
		unix.LANDLOCK_CREATE_RULESET_VERSION,
	)
	if errno != 0 {
		if errors.Is(errno, unix.ENOSYS) || errors.Is(errno, unix.EOPNOTSUPP) {
			return 0, fmt.Errorf("Landlock unavailable: %w", errno)
		}
		return 0, fmt.Errorf("query Landlock ABI: %w", errno)
	}
	if version < 1 {
		return 0, fmt.Errorf("unsupported Landlock ABI %d", version)
	}
	return int(version), nil
}

func addLandlockPath(ruleset int, path string, allowed uint64) error {
	descriptor, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open Landlock path %s: %w", path, err)
	}
	defer unix.Close(descriptor)

	var info unix.Stat_t
	if err := unix.Fstat(descriptor, &info); err != nil {
		return fmt.Errorf("fstat Landlock path %s: %w", path, err)
	}
	if info.Mode&unix.S_IFMT != unix.S_IFDIR {
		allowed &^= unix.LANDLOCK_ACCESS_FS_READ_DIR
	}
	attributes := unix.LandlockPathBeneathAttr{
		Allowed_access: allowed,
		Parent_fd:      int32(descriptor),
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_LANDLOCK_ADD_RULE,
		uintptr(ruleset),
		unix.LANDLOCK_RULE_PATH_BENEATH,
		uintptr(unsafe.Pointer(&attributes)),
		0,
		0,
		0,
	)
	if errno != 0 {
		return fmt.Errorf("add Landlock path %s: %w", path, errno)
	}
	return nil
}
