package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSafeExtension(t *testing.T) {
	for _, extension := range []string{".pdf", ".docx", ".xlsx", ".jpeg"} {
		if !safeExtension(extension) {
			t.Errorf("safeExtension(%q) = false", extension)
		}
	}
	for _, extension := range []string{"", ".", ".../../secret", ".PDF", ".reallylongextension", ".doc-x"} {
		if safeExtension(extension) {
			t.Errorf("safeExtension(%q) = true", extension)
		}
	}
}

func TestRandomNameFailsClosedWithoutEntropy(t *testing.T) {
	if _, err := randomNameFrom(strings.NewReader("short"), "report.pdf"); err == nil {
		t.Fatal("randomNameFrom() accepted insufficient entropy")
	}

	name, err := randomNameFrom(strings.NewReader("0123456789abcdef"), "../REPORT.PDF")
	if err != nil {
		t.Fatal(err)
	}
	if name != "30313233343536373839616263646566.pdf" {
		t.Fatalf("randomNameFrom() = %q", name)
	}

	name, err = randomNameFrom(strings.NewReader("0123456789abcdef"), "extensionless")
	if err != nil {
		t.Fatal(err)
	}
	if name != "30313233343536373839616263646566.pdf" {
		t.Fatalf("extensionless randomNameFrom() = %q", name)
	}

	name, err = randomNameFrom(strings.NewReader("0123456789abcdef"), "report.doc-x")
	if err != nil {
		t.Fatal(err)
	}
	if name != "30313233343536373839616263646566.bin" {
		t.Fatalf("unsafe-extension randomNameFrom() = %q", name)
	}

	failing := errorReader{err: errors.New("entropy source failed")}
	if _, err := randomNameFrom(failing, "report.pdf"); err == nil {
		t.Fatal("randomNameFrom() ignored entropy source failure")
	}
}

type errorReader struct{ err error }

func (reader errorReader) Read([]byte) (int, error) { return 0, reader.err }

func TestConvertUploadedFilesIsOrderedAndBounded(t *testing.T) {
	files := []uploadedFile{{name: "0"}, {name: "1"}, {name: "2"}, {name: "3"}}
	var active atomic.Int32
	var maximum atomic.Int32
	convert := func(_ context.Context, _ []byte, filename, _ string) (ConvertResult, error) {
		current := active.Add(1)
		for {
			previous := maximum.Load()
			if current <= previous || maximum.CompareAndSwap(previous, current) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		active.Add(-1)
		return ConvertResult{MDContent: filename}, nil
	}

	results, err := convertUploadedFilesWith(context.Background(), files, "raw", convert)
	if err != nil {
		t.Fatal(err)
	}
	if maximum.Load() != 2 {
		t.Fatalf("maximum concurrency = %d, want 2", maximum.Load())
	}
	for index, result := range results {
		if result.MDContent != files[index].name {
			t.Fatalf("result %d = %q, want %q", index, result.MDContent, files[index].name)
		}
	}
}

func TestHealthStatusRequiresParserButNotOptionalVLM(t *testing.T) {
	tests := []struct {
		parser bool
		vlm    bool
		status string
		code   int
	}{
		{true, true, "ok", http.StatusOK},
		{true, false, "degraded", http.StatusOK},
		{false, true, "unavailable", http.StatusServiceUnavailable},
		{false, false, "unavailable", http.StatusServiceUnavailable},
	}
	for _, test := range tests {
		status, code := healthStatus(test.parser, test.vlm)
		if status != test.status || code != test.code {
			t.Fatalf("healthStatus(%v, %v) = %q, %d", test.parser, test.vlm, status, code)
		}
	}
}

func TestNormalizeExtractResultUsesMarkdownFallback(t *testing.T) {
	result := ExtractResult{Pages: []ExtractPage{
		{Text: "pdf text", MDContent: "markdown should not replace text"},
		{MDContent: "spreadsheet markdown"},
	}}

	normalizeExtractResult(&result)

	if result.Pages[0].Text != "pdf text" {
		t.Fatalf("existing text was replaced: %q", result.Pages[0].Text)
	}
	if result.Pages[1].Text != "spreadsheet markdown" {
		t.Fatalf("markdown fallback missing: %q", result.Pages[1].Text)
	}
}
