package parser

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tinfoilsh/confidential-doc-upload/internal/processing"
)

func TestLimitedBufferCapsMemoryAndSignals(t *testing.T) {
	buffer := newLimitedBuffer(4)
	if count, err := buffer.Write([]byte("abcdef")); err != nil || count != 6 {
		t.Fatalf("Write() = %d, %v", count, err)
	}
	if got := buffer.String(); got != "abcd" {
		t.Fatalf("buffer = %q, want abcd", got)
	}
	if !buffer.wasExceeded.Load() {
		t.Fatal("output limit was not recorded")
	}
	select {
	case <-buffer.exceeded:
	default:
		t.Fatal("output limit signal was not closed")
	}
}

func TestParserProcessHelper(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "--parser-error-fixture" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "success":
		fmt.Print("safe result")
	case "failure":
		fmt.Fprint(os.Stderr, "private@example.test document contents")
		os.Exit(1)
	case "stdout":
		fmt.Print(strings.Repeat("x", 1024))
	case "stderr":
		fmt.Fprint(os.Stderr, strings.Repeat("x", maxStderrBytes+1))
	case "timeout", "canceled":
		time.Sleep(time.Hour)
	}
	os.Exit(0)
}

func TestParserProcessFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want processing.Code
	}{
		{"failure", processing.ParserFailed},
		{"stdout", processing.ParserOutputLimit},
		{"stderr", processing.ParserOutputLimit},
		{"timeout", processing.ParserTimeout},
		{"canceled", processing.Canceled},
		{"success", ""},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if tc.mode == "timeout" {
				var stop context.CancelFunc
				ctx, stop = context.WithTimeout(ctx, 100*time.Millisecond)
				defer stop()
			}
			if tc.mode == "canceled" {
				cancel()
			}
			cmd := exec.Command(os.Args[0], "-test.run=^TestParserProcessHelper$", "--", "--parser-error-fixture", tc.mode)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			output, err := runParserProcess(ctx, cmd, 32)
			if tc.want == "" {
				if err != nil || string(output) != "safe result" {
					t.Fatalf("successful parser: %q, %v", output, err)
				}
				return
			}
			if err == nil || processing.CodeOf(err) != tc.want || output != nil {
				t.Fatalf("parser error = %v, want %s", err, tc.want)
			}
			if strings.Contains(err.Error(), "private@example.test") {
				t.Fatal("parser stderr leaked")
			}
			if cmd.ProcessState == nil || cmd.ProcessState.Pid() == 0 {
				t.Fatal("parser process was not reaped")
			}
		})
	}
}

func TestParserEmptyAndUnavailableClassification(t *testing.T) {
	_, err := Run(context.Background(), nil, "private.pdf", Extract, 100)
	if processing.CodeOf(err) != processing.Empty {
		t.Fatalf("empty document: %v", err)
	}
	_, err = runParserProcess(context.Background(), exec.Command("/nonexistent/parser-fixture"), 32)
	if processing.CodeOf(err) != processing.ParserUnavailable {
		t.Fatalf("unavailable parser: %v", err)
	}
}

func TestParserCommandSelectsOneParser(t *testing.T) {
	pdf := parserCommand("random.pdf", Render, 144)
	if strings.Join(pdf, " ") != pdfParserBin+" --render --dpi=144" {
		t.Fatalf("PDF command = %q", pdf)
	}
	document := parserCommand("-random.docx", Extract, 0)
	if len(document) != 5 || document[0] != pythonBin || document[2] != docParserPath || document[3] != string(Extract) || document[4] != "--filename=-random.docx" {
		t.Fatalf("document command = %q", document)
	}
}
