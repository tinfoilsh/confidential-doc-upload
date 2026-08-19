// sandbox-exec installs hard resource and syscall restrictions, then replaces
// itself with exactly one document parser process.
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"

	"github.com/tinfoilsh/confidential-doc-upload/internal/sandbox"
	"golang.org/x/sys/unix"
)

func main() {
	// Landlock credentials are per-thread. Keep setup and execve on the same OS
	// thread; execve removes all of the Go runtime's other threads.
	runtime.LockOSThread()

	memoryMB := flag.Uint64("memory-mb", 1536, "hard virtual-address-space limit in MiB")
	cpuSeconds := flag.Uint64("cpu-seconds", 120, "hard CPU-time limit")
	openFiles := flag.Uint64("open-files", 64, "hard open-file limit")
	flag.Parse()
	command := flag.Args()
	if len(command) == 0 || command[0] == "" || command[0][0] != '/' {
		fatal("an absolute parser command is required after --")
	}
	if *memoryMB < 1024 || *memoryMB > 4096 {
		fatal("memory-mb must be between 1024 and 4096")
	}
	if *cpuSeconds < 1 || *cpuSeconds > 300 {
		fatal("cpu-seconds must be between 1 and 300")
	}
	if *openFiles < 16 || *openFiles > 256 {
		fatal("open-files must be between 16 and 256")
	}

	// execve resets dumpability for ordinary executables, so each parser also
	// reasserts this after exec. Protect the wrapper during the transition too.
	if err := sandbox.ProtectProcess(); err != nil {
		fatal("protect sandbox process: %v", err)
	}
	if err := sandbox.ApplyLimits(sandbox.Limits{
		AddressSpaceBytes: *memoryMB * 1024 * 1024,
		CPUSeconds:        *cpuSeconds,
		OpenFiles:         *openFiles,
	}); err != nil {
		fatal("apply resource limits: %v", err)
	}
	if err := sandbox.RestrictParserFilesystem(command[0]); err != nil {
		fatal("install parser filesystem sandbox: %v", err)
	}
	if err := sandbox.RestrictParser(); err != nil {
		fatal("install parser sandbox: %v", err)
	}
	if err := unix.Exec(command[0], command, []string{}); err != nil {
		fatal("exec parser: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "sandbox-exec: "+format+"\n", args...)
	os.Exit(127)
}
