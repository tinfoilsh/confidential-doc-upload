package parser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	sandboxExecBin = "/usr/local/bin/sandbox-exec"
	pdfParserBin   = "/usr/local/bin/pdfparser"
	pythonBin      = "/usr/local/bin/python3"
	docParserPath  = "/app/docparser.py"

	defaultTimeoutSeconds = 120
	defaultMemoryMB       = 1536
	defaultMaxOutputMB    = 256
	maxStderrBytes        = 1024 * 1024
)

type Operation string

const (
	Extract Operation = "extract"
	Render  Operation = "render"
)

func Run(ctx context.Context, data []byte, filename string, operation Operation, dpi int) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("empty document")
	}
	if operation != Extract && operation != Render {
		return nil, fmt.Errorf("unsupported parser operation %q", operation)
	}
	if operation == Render && (dpi < 20 || dpi > 600) {
		return nil, fmt.Errorf("dpi %d outside 20..600", dpi)
	}

	timeoutSeconds := boundedEnvInt("PARSER_TIMEOUT_SECONDS", defaultTimeoutSeconds, 1, 300)
	memoryMB := boundedEnvInt("PARSER_MEMORY_LIMIT_MB", defaultMemoryMB, 1024, 4096)
	maxOutputMB := boundedEnvInt("PARSER_MAX_OUTPUT_MB", defaultMaxOutputMB, 1, 256)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
	defer cancel()

	command := parserCommand(filename, operation, dpi)
	args := []string{
		fmt.Sprintf("--memory-mb=%d", memoryMB),
		fmt.Sprintf("--cpu-seconds=%d", timeoutSeconds),
		"--open-files=64",
		"--",
	}
	args = append(args, command...)
	cmd := exec.Command(sandboxExecBin, args...)
	cmd.Stdin = bytes.NewReader(data)
	cmd.Env = []string{}
	cmd.Dir = "/"
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	cmd.WaitDelay = 2 * time.Second

	stdout := newLimitedBuffer(maxOutputMB * 1024 * 1024)
	stderr := newLimitedBuffer(maxStderrBytes)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start parser: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var runErr error
	var limitName string
	select {
	case runErr = <-done:
	case <-ctx.Done():
		killProcessGroup(cmd)
		runErr = <-done
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("parser timeout after %ds", timeoutSeconds)
		}
		return nil, ctx.Err()
	case <-stdout.exceeded:
		limitName = "stdout"
		killProcessGroup(cmd)
		runErr = <-done
	case <-stderr.exceeded:
		limitName = "stderr"
		killProcessGroup(cmd)
		runErr = <-done
	}
	if stdout.wasExceeded.Load() {
		limitName = "stdout"
	} else if stderr.wasExceeded.Load() {
		limitName = "stderr"
	}

	if limitName != "" {
		return nil, fmt.Errorf("parser %s exceeded hard output limit", limitName)
	}
	if runErr != nil {
		return nil, fmt.Errorf("parser failed: %w", runErr)
	}
	return append([]byte(nil), stdout.Bytes()...), nil
}

func parserCommand(filename string, operation Operation, dpi int) []string {
	if strings.HasSuffix(strings.ToLower(filename), ".pdf") {
		command := []string{pdfParserBin}
		if operation == Render {
			command = append(command, "--render", fmt.Sprintf("--dpi=%d", dpi))
		}
		return command
	}
	command := []string{pythonBin, "-B", docParserPath, string(operation), "--filename", filename}
	if operation == Render {
		command = append(command, fmt.Sprintf("--dpi=%d", dpi))
	}
	return command
}

func killProcessGroup(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if err != nil && !errors.Is(err, syscall.ESRCH) {
		_ = cmd.Process.Kill()
	}
}

func boundedEnvInt(name string, fallback, minimum, maximum int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < minimum || value > maximum {
		return fallback
	}
	return value
}

type limitedBuffer struct {
	buffer      bytes.Buffer
	limit       int
	exceeded    chan struct{}
	once        sync.Once
	wasExceeded atomic.Bool
}

func newLimitedBuffer(limit int) *limitedBuffer {
	return &limitedBuffer{limit: limit, exceeded: make(chan struct{})}
}

func (buffer *limitedBuffer) Write(data []byte) (int, error) {
	originalLength := len(data)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining > 0 {
		if len(data) > remaining {
			data = data[:remaining]
		}
		_, _ = buffer.buffer.Write(data)
	}
	if originalLength > remaining {
		buffer.once.Do(func() {
			buffer.wasExceeded.Store(true)
			close(buffer.exceeded)
		})
	}
	return originalLength, nil
}

func (buffer *limitedBuffer) Bytes() []byte  { return buffer.buffer.Bytes() }
func (buffer *limitedBuffer) String() string { return buffer.buffer.String() }
