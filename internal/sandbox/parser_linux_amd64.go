package sandbox

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	seccompDataNumberOffset = 0
	seccompDataArchOffset   = 4
	seccompDataArgsOffset   = 16
	x32SyscallBit           = 0x40000000
)

// O_TMPFILE is intentionally not included directly because its numeric value
// also contains O_DIRECTORY. A valid O_TMPFILE open is already caught by its
// mandatory O_RDWR or O_WRONLY bit.
const writeOpenFlags = unix.O_WRONLY | unix.O_RDWR | unix.O_CREAT | unix.O_TRUNC | unix.O_APPEND

var networkSyscalls = []uint32{
	unix.SYS_SOCKET,
	unix.SYS_SOCKETPAIR,
	unix.SYS_CONNECT,
	unix.SYS_BIND,
	unix.SYS_LISTEN,
	unix.SYS_ACCEPT,
	unix.SYS_ACCEPT4,
	unix.SYS_SENDTO,
	unix.SYS_RECVFROM,
	unix.SYS_SENDMSG,
	unix.SYS_RECVMSG,
	unix.SYS_SENDMMSG,
	unix.SYS_RECVMMSG,
	unix.SYS_GETSOCKNAME,
	unix.SYS_GETPEERNAME,
	unix.SYS_SETSOCKOPT,
	unix.SYS_GETSOCKOPT,
	unix.SYS_SHUTDOWN,
	unix.SYS_IO_URING_SETUP,
	unix.SYS_IO_URING_ENTER,
	unix.SYS_IO_URING_REGISTER,
}

// These calls can create a process outside the parser's thread group, escape
// its process group, inspect or signal another process, or create a namespace.
// Parser threads remain available through clone(CLONE_THREAD).
var processIsolationSyscalls = []uint32{
	unix.SYS_FORK,
	unix.SYS_VFORK,
	unix.SYS_CLONE3,
	unix.SYS_KILL,
	unix.SYS_TKILL,
	unix.SYS_RT_SIGQUEUEINFO,
	unix.SYS_RT_TGSIGQUEUEINFO,
	unix.SYS_PIDFD_OPEN,
	unix.SYS_PIDFD_GETFD,
	unix.SYS_PIDFD_SEND_SIGNAL,
	unix.SYS_PTRACE,
	unix.SYS_PROCESS_VM_READV,
	unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_PROCESS_MADVISE,
	unix.SYS_PROCESS_MRELEASE,
	unix.SYS_KCMP,
	unix.SYS_SETNS,
	unix.SYS_UNSHARE,
	unix.SYS_SETSID,
	unix.SYS_SETPGID,
	unix.SYS_SCHED_SETAFFINITY,
	unix.SYS_SCHED_SETPARAM,
	unix.SYS_SCHED_SETSCHEDULER,
	unix.SYS_SCHED_SETATTR,
	unix.SYS_SETPRIORITY,
	unix.SYS_IOPRIO_SET,
}

// The parser image is read-only. Denying namespace, mount, and path mutation
// syscalls also protects the shared Unix-socket volume from a parser RCE.
var filesystemMutationSyscalls = []uint32{
	unix.SYS_CREAT,
	unix.SYS_OPENAT2,
	unix.SYS_UNLINK,
	unix.SYS_UNLINKAT,
	unix.SYS_RENAME,
	unix.SYS_RENAMEAT,
	unix.SYS_RENAMEAT2,
	unix.SYS_MKDIR,
	unix.SYS_MKDIRAT,
	unix.SYS_RMDIR,
	unix.SYS_LINK,
	unix.SYS_LINKAT,
	unix.SYS_SYMLINK,
	unix.SYS_SYMLINKAT,
	unix.SYS_MKNOD,
	unix.SYS_MKNODAT,
	unix.SYS_CHMOD,
	unix.SYS_FCHMOD,
	unix.SYS_FCHMODAT,
	unix.SYS_FCHMODAT2,
	unix.SYS_CHOWN,
	unix.SYS_FCHOWN,
	unix.SYS_LCHOWN,
	unix.SYS_FCHOWNAT,
	unix.SYS_TRUNCATE,
	unix.SYS_FTRUNCATE,
	unix.SYS_FALLOCATE,
	unix.SYS_UTIME,
	unix.SYS_UTIMES,
	unix.SYS_FUTIMESAT,
	unix.SYS_UTIMENSAT,
	unix.SYS_SETXATTR,
	unix.SYS_LSETXATTR,
	unix.SYS_FSETXATTR,
	unix.SYS_REMOVEXATTR,
	unix.SYS_LREMOVEXATTR,
	unix.SYS_FREMOVEXATTR,
	unix.SYS_MOUNT,
	unix.SYS_UMOUNT2,
	unix.SYS_PIVOT_ROOT,
	unix.SYS_CHROOT,
	unix.SYS_MOVE_MOUNT,
	unix.SYS_OPEN_TREE,
	unix.SYS_FSOPEN,
	unix.SYS_FSCONFIG,
	unix.SYS_FSMOUNT,
	unix.SYS_FSPICK,
	unix.SYS_MOUNT_SETATTR,
	unix.SYS_OPEN_BY_HANDLE_AT,
}

var kernelAttackSurfaceSyscalls = []uint32{
	unix.SYS_BPF,
	unix.SYS_PERF_EVENT_OPEN,
	unix.SYS_USERFAULTFD,
	unix.SYS_KEYCTL,
	unix.SYS_ADD_KEY,
	unix.SYS_REQUEST_KEY,
	unix.SYS_REBOOT,
	unix.SYS_KEXEC_LOAD,
	unix.SYS_KEXEC_FILE_LOAD,
	unix.SYS_INIT_MODULE,
	unix.SYS_FINIT_MODULE,
	unix.SYS_DELETE_MODULE,
	unix.SYS_ACCT,
	unix.SYS_SWAPON,
	unix.SYS_SWAPOFF,
	unix.SYS_QUOTACTL,
	unix.SYS_MEMFD_CREATE,
	unix.SYS_SHMGET,
	unix.SYS_SHMAT,
	unix.SYS_SHMCTL,
	unix.SYS_MSGGET,
	unix.SYS_MSGSND,
	unix.SYS_MSGRCV,
	unix.SYS_MSGCTL,
	unix.SYS_SEMGET,
	unix.SYS_SEMOP,
	unix.SYS_SEMCTL,
	unix.SYS_SEMTIMEDOP,
	unix.SYS_MQ_OPEN,
	unix.SYS_MQ_UNLINK,
	unix.SYS_MQ_TIMEDSEND,
	unix.SYS_MQ_TIMEDRECEIVE,
	unix.SYS_MQ_NOTIFY,
	unix.SYS_MQ_GETSETATTR,
}

// RestrictParser installs an irreversible, process-wide seccomp policy before
// untrusted document bytes are read. Installation is synchronized to every
// existing thread, and future threads inherit the filter.
func RestrictParser() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges: %w", err)
	}

	filters := parserFilters(uint32(unix.Getpid()))
	program := unix.SockFprog{
		Len:    uint16(len(filters)),
		Filter: &filters[0],
	}
	result, _, errno := unix.Syscall(
		unix.SYS_SECCOMP,
		unix.SECCOMP_SET_MODE_FILTER,
		unix.SECCOMP_FILTER_FLAG_TSYNC,
		uintptr(unsafe.Pointer(&program)),
	)
	if errno != 0 {
		return fmt.Errorf("install parser seccomp filter: %w", errno)
	}
	if result != 0 {
		return fmt.Errorf("install parser seccomp filter: thread %d rejected synchronization", result)
	}
	return nil
}

// RestrictNetwork is retained for callers built against the original API.
func RestrictNetwork() error { return RestrictParser() }

func parserFilters(processID uint32) []unix.SockFilter {
	filters := []unix.SockFilter{
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArchOffset),
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.AUDIT_ARCH_X86_64, 1, 0),
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_KILL_PROCESS),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataNumberOffset),
		jump(unix.BPF_JMP|unix.BPF_JGE|unix.BPF_K, x32SyscallBit, 0, 1),
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.ENOSYS)),
	}

	// clone is permitted only for threads in this parser process. clone3 is
	// denied below because classic seccomp cannot safely inspect its pointer
	// argument.
	filters = appendArgumentMaskRule(filters, unix.SYS_CLONE, 0, unix.CLONE_THREAD, true)
	// Go uses tgkill for runtime signals. The kernel verifies that the supplied
	// TID belongs to the supplied TGID, so constrain TGID to this process.
	filters = appendArgumentEqualRule(filters, unix.SYS_TGKILL, 0, processID)
	// libc and language runtimes query their own limits with pid=0. An explicit
	// PID could target a briefly dumpable sibling during exec startup, so never
	// permit cross-process prlimit64.
	filters = appendArgumentEqualRule(filters, unix.SYS_PRLIMIT64, 0, 0)
	// Dynamic loaders and document libraries may read files, but cannot acquire
	// a writable descriptor.
	filters = appendArgumentMaskRule(filters, unix.SYS_OPEN, 1, writeOpenFlags, false)
	filters = appendArgumentMaskRule(filters, unix.SYS_OPENAT, 2, writeOpenFlags, false)

	denied := make([]uint32, 0, len(networkSyscalls)+len(processIsolationSyscalls)+len(filesystemMutationSyscalls)+len(kernelAttackSurfaceSyscalls))
	denied = append(denied, networkSyscalls...)
	denied = append(denied, processIsolationSyscalls...)
	denied = append(denied, filesystemMutationSyscalls...)
	denied = append(denied, kernelAttackSurfaceSyscalls...)
	for _, number := range denied {
		filters = append(filters,
			jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, number, 0, 1),
			stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)),
		)
	}
	return append(filters, stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW))
}

// appendArgumentMaskRule allows a syscall when the selected 32-bit argument
// contains (requireSet=true) or does not contain (requireSet=false) mask.
func appendArgumentMaskRule(filters []unix.SockFilter, syscallNumber uint32, argument uint32, mask uint32, requireSet bool) []unix.SockFilter {
	filters = append(filters,
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, syscallNumber, 0, 4),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArgsOffset+argument*8),
	)
	if requireSet {
		filters = append(filters, jump(unix.BPF_JMP|unix.BPF_JSET|unix.BPF_K, mask, 0, 1))
	} else {
		filters = append(filters, jump(unix.BPF_JMP|unix.BPF_JSET|unix.BPF_K, mask, 1, 0))
	}
	filters = append(filters,
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataNumberOffset),
	)
	return filters
}

func appendArgumentEqualRule(filters []unix.SockFilter, syscallNumber uint32, argument uint32, value uint32) []unix.SockFilter {
	return append(filters,
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, syscallNumber, 0, 4),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArgsOffset+argument*8),
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, value, 0, 1),
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM)),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataNumberOffset),
	)
}

func stmt(code uint16, value uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: value}
}

func jump(code uint16, value uint32, onTrue, onFalse uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, K: value, Jt: onTrue, Jf: onFalse}
}
