package parser

import (
	"strings"
	"testing"
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

func TestParserCommandSelectsOneParser(t *testing.T) {
	pdf := parserCommand("random.pdf", Render, 144)
	if strings.Join(pdf, " ") != pdfParserBin+" --render --dpi=144" {
		t.Fatalf("PDF command = %q", pdf)
	}
	document := parserCommand("random.docx", Extract, 0)
	if len(document) != 6 || document[0] != pythonBin || document[2] != docParserPath || document[3] != string(Extract) {
		t.Fatalf("document command = %q", document)
	}
}
