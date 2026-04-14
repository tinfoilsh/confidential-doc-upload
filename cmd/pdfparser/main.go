package main

import (
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/tinfoilsh/confidential-doc-upload/internal/mupdf"
	"github.com/tinfoilsh/confidential-doc-upload/internal/pdftomd"
)

type pageOutput struct {
	Page      int    `json:"page"`
	MDContent string `json:"md_content"`
	IsScanned bool   `json:"is_scanned"`
	Image     string `json:"image,omitempty"`
}

type parseOutput struct {
	Format    string       `json:"format"`
	Pages     []pageOutput `json:"pages"`
	PageCount int          `json:"page_count"`
}

func main() {
	render := flag.Bool("render", false, "include page images as base64 PNG")
	dpi := flag.Int("dpi", 100, "DPI for page rendering")
	flag.Parse()

	// Use Go's soft memory limit (GOMEMLIMIT) for defense in depth.
	// This is Go-aware and doesn't break the runtime like RLIMIT_AS does.
	// The router also sets a hard 120s timeout.
	debug.SetMemoryLimit(512 * 1024 * 1024)

	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		fatal("read stdin: %v", err)
	}
	if len(data) == 0 {
		fatal("empty input")
	}

	doc, err := mupdf.OpenFromBytes(data)
	if err != nil {
		fatal("open document: %v", err)
	}
	defer doc.Close()

	results, err := pdftomd.ConvertDocument(doc)
	if err != nil {
		fatal("convert: %v", err)
	}

	output := parseOutput{
		Format:    "pdf",
		PageCount: len(results),
	}

	for _, r := range results {
		po := pageOutput{
			Page:      r.PageNum,
			MDContent: r.Markdown,
			IsScanned: r.IsScanned,
		}
		if *render {
			png, err := doc.RenderPagePNG(r.PageNum-1, *dpi)
			if err == nil && len(png) > 0 {
				po.Image = base64.StdEncoding.EncodeToString(png)
			}
		}
		output.Pages = append(output.Pages, po)
	}

	if err := json.NewEncoder(os.Stdout).Encode(output); err != nil {
		fatal("encode output: %v", err)
	}
}

func fatal(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "pdfparser: "+format+"\n", args...)
	os.Exit(1)
}

