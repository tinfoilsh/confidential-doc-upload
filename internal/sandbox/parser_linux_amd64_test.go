package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRestrictParserEnforcesRuntimePolicy(t *testing.T) {
	if os.Getenv("TEST_PARSER_SANDBOX_HELPER") == "1" {
		runParserSandboxAssertions()
		os.Exit(0)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestRestrictParserEnforcesRuntimePolicy$")
	command.Env = []string{"TEST_PARSER_SANDBOX_HELPER=1"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sandbox helper failed: %v\n%s", err, output)
	}
}

func runParserSandboxAssertions() {
	if err := RestrictParser(); err != nil {
		os.Exit(10)
	}

	if fd, err := unix.Open("/dev/null", unix.O_RDONLY|unix.O_CLOEXEC, 0); err != nil {
		os.Exit(11)
	} else {
		unix.Close(fd)
	}
	if fd, err := unix.Open("/tmp/parser-sandbox-write", unix.O_WRONLY|unix.O_CREAT|unix.O_CLOEXEC, 0600); fd >= 0 || !errors.Is(err, unix.EPERM) {
		if fd >= 0 {
			unix.Close(fd)
		}
		os.Exit(12)
	}

	if fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0); fd >= 0 || !errors.Is(err, unix.EPERM) {
		if fd >= 0 {
			unix.Close(fd)
		}
		os.Exit(13)
	}
	if _, _, errno := unix.Syscall(unix.SYS_IO_URING_SETUP, 1, 0, 0); errno != unix.EPERM {
		os.Exit(14)
	}
	if _, _, errno := unix.RawSyscall(unix.SYS_FORK, 0, 0, 0); errno != unix.EPERM {
		os.Exit(15)
	}
	if _, _, errno := unix.RawSyscall(unix.SYS_CLONE3, 0, 0, 0); errno != unix.EPERM {
		os.Exit(16)
	}
	if err := unix.Kill(os.Getppid(), 0); !errors.Is(err, unix.EPERM) {
		os.Exit(17)
	}
	if err := unix.Tgkill(os.Getpid(), unix.Gettid(), 0); err != nil {
		os.Exit(18)
	}
	if err := unix.Prlimit(os.Getppid(), unix.RLIMIT_NOFILE, nil, nil); !errors.Is(err, unix.EPERM) {
		os.Exit(19)
	}
	if err := unix.Unshare(unix.CLONE_NEWUSER); !errors.Is(err, unix.EPERM) {
		os.Exit(20)
	}
	if err := unix.Unlink("/tmp/does-not-exist"); !errors.Is(err, unix.EPERM) {
		os.Exit(21)
	}
	if err := unix.Prctl(unix.PR_SET_PDEATHSIG, 0, 0, 0, 0); !errors.Is(err, unix.EPERM) {
		os.Exit(22)
	}
	if err := unix.Prctl(unix.PR_SET_DUMPABLE, 0, 0, 0, 0); err != nil {
		os.Exit(23)
	}
}

func TestParserFiltersRejectEveryDeniedSyscall(t *testing.T) {
	filters := parserFilters(uint32(os.Getpid()))
	denied := append([]uint32{}, networkSyscalls...)
	denied = append(denied, processIsolationSyscalls...)
	denied = append(denied, filesystemMutationSyscalls...)
	denied = append(denied, kernelAttackSurfaceSyscalls...)
	for _, syscallNumber := range denied {
		found := false
		for index := 0; index+1 < len(filters); index++ {
			if filters[index].Code == unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K &&
				filters[index].K == syscallNumber &&
				filters[index+1].Code == unix.BPF_RET|unix.BPF_K &&
				filters[index+1].K == unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("syscall %d is not denied", syscallNumber)
		}
	}
}
