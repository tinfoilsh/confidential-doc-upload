package sandbox

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

func TestProtectProcessHidesEnvironmentFromChild(t *testing.T) {
	switch os.Getenv("TEST_PROTECT_PROCESS_STAGE") {
	case "parent":
		if err := ProtectProcess(); err != nil {
			os.Exit(21)
		}
		child := exec.Command(os.Args[0], "-test.run=^TestProtectProcessHidesEnvironmentFromChild$")
		child.Env = []string{
			"TEST_PROTECT_PROCESS_STAGE=child",
			"TEST_PROTECTED_PARENT_PID=" + strconv.Itoa(os.Getpid()),
		}
		if err := child.Run(); err != nil {
			os.Exit(22)
		}
		os.Exit(0)
	case "child":
		parentPID := os.Getenv("TEST_PROTECTED_PARENT_PID")
		data, err := os.ReadFile("/proc/" + parentPID + "/environ")
		if bytes.Contains(data, []byte("REVIEW_FAKE_SECRET=must-not-be-readable")) {
			os.Exit(23)
		}
		if err != nil && !errors.Is(err, os.ErrPermission) {
			os.Exit(24)
		}
		os.Exit(0)
	}

	parent := exec.Command(os.Args[0], "-test.run=^TestProtectProcessHidesEnvironmentFromChild$")
	parent.Env = []string{
		"TEST_PROTECT_PROCESS_STAGE=parent",
		"REVIEW_FAKE_SECRET=must-not-be-readable",
	}
	if output, err := parent.CombinedOutput(); err != nil {
		t.Fatalf("protected-process helper failed: %v\n%s", err, output)
	}
}
