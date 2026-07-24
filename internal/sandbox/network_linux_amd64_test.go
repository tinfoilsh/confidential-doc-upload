package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRestrictNetworkRejectsSockets(t *testing.T) {
	if os.Getenv("TEST_NETWORK_SANDBOX_HELPER") == "1" {
		if err := RestrictNetwork(); err != nil {
			os.Exit(2)
		}
		fd, err := unix.Socket(unix.AF_INET, unix.SOCK_STREAM, 0)
		if fd >= 0 {
			unix.Close(fd)
			os.Exit(3)
		}
		if !errors.Is(err, unix.EPERM) {
			os.Exit(4)
		}
		os.Exit(0)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestRestrictNetworkRejectsSockets$")
	command.Env = append(os.Environ(), "TEST_NETWORK_SANDBOX_HELPER=1")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("sandbox helper failed: %v\n%s", err, output)
	}
}

func TestNetworkFiltersRejectEveryNetworkSyscall(t *testing.T) {
	filters := networkFilters()
	for _, syscallNumber := range networkSyscalls {
		found := false
		for index := 6; index+1 < len(filters); index += 2 {
			if filters[index].K == syscallNumber && filters[index+1].K == unix.SECCOMP_RET_ERRNO|uint32(unix.EPERM) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("network syscall %d is not denied", syscallNumber)
		}
	}
}
