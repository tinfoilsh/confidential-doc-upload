package sandbox

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRestrictFilesystemEnforcesAllowlist(t *testing.T) {
	if os.Getenv("TEST_LANDLOCK_HELPER") == "1" {
		runtime.LockOSThread()
		if err := restrictFilesystem([]filesystemRule{{path: "/etc/hosts"}}); err != nil {
			os.Exit(10)
		}
		if _, err := os.ReadFile("/etc/hosts"); err != nil {
			os.Exit(11)
		}
		if _, err := os.ReadFile("/proc/self/status"); !errors.Is(err, unix.EACCES) {
			os.Exit(12)
		}
		if err := os.WriteFile("/tmp/landlock-write", []byte("denied"), 0600); !errors.Is(err, unix.EACCES) {
			os.Exit(13)
		}
		os.Exit(0)
	}
	if _, err := landlockABI(); err != nil {
		if errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) {
			t.Skipf("host kernel does not support Landlock: %v", err)
		}
		t.Fatalf("query Landlock support: %v", err)
	}

	command := exec.Command(os.Args[0], "-test.run=^TestRestrictFilesystemEnforcesAllowlist$")
	command.Env = []string{"TEST_LANDLOCK_HELPER=1"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("Landlock helper failed: %v\n%s", err, output)
	}
}

func TestParserRuntimePathsExposeOnlySelectedParser(t *testing.T) {
	assertExecutable := func(rules []filesystemRule, path string, wanted bool) {
		t.Helper()
		for _, rule := range rules {
			if rule.path == path && rule.executable {
				if !wanted {
					t.Fatalf("%s unexpectedly executable", path)
				}
				return
			}
		}
		if wanted {
			t.Fatalf("%s is not executable", path)
		}
	}
	assertAbsent := func(rules []filesystemRule, path string) {
		t.Helper()
		for _, rule := range rules {
			if rule.path == path {
				t.Fatalf("%s unexpectedly visible in parser sandbox", path)
			}
		}
	}

	pdfRules, err := parserRuntimePaths(pdfParserExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if len(pdfRules) != 2 {
		t.Fatalf("PDF sandbox has %d paths, want only loader and parser", len(pdfRules))
	}
	assertExecutable(pdfRules, pdfParserExecutable, true)
	assertAbsent(pdfRules, pythonExecutable)

	pythonRules, err := parserRuntimePaths(pythonExecutable)
	if err != nil {
		t.Fatal(err)
	}
	if len(pythonRules) != 7 {
		t.Fatalf("Python sandbox has %d paths, want only loader, parser, and runtime", len(pythonRules))
	}
	assertExecutable(pythonRules, pythonExecutable, true)
	assertAbsent(pythonRules, pdfParserExecutable)

	if _, err := parserRuntimePaths("/bin/sh"); err == nil {
		t.Fatal("parserRuntimePaths() accepted an unsupported executable")
	}
}
