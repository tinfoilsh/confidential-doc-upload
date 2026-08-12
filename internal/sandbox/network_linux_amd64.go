package sandbox

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	seccompDataNumberOffset = 0
	seccompDataArchOffset   = 4
	x32SyscallBit           = 0x40000000
)

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

// RestrictNetwork installs a process-wide seccomp filter that rejects network
// syscalls. The filter is synchronized onto every existing process thread.
func RestrictNetwork() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges: %w", err)
	}

	filters := networkFilters()
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
		return fmt.Errorf("install network seccomp filter: %w", errno)
	}
	if result != 0 {
		return fmt.Errorf("install network seccomp filter: thread %d rejected synchronization", result)
	}
	return nil
}

func networkFilters() []unix.SockFilter {
	filters := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataArchOffset},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jt: 1, K: unix.AUDIT_ARCH_X86_64},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataNumberOffset},
		{Code: unix.BPF_JMP | unix.BPF_JGE | unix.BPF_K, Jf: 1, K: x32SyscallBit},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)},
	}
	for _, number := range networkSyscalls {
		filters = append(filters,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: number},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	return append(filters, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
}
