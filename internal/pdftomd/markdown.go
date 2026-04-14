package pdftomd

import (
	"strings"

	"github.com/tinfoilsh/confidential-doc-upload/internal/mupdf"
)

const scannedThreshold = 50

// PageResult holds the markdown output and metadata for one page.
type PageResult struct {
	Markdown  string `json:"md_content"`
	IsScanned bool   `json:"is_scanned"`
	PageNum   int    `json:"page"`
}

// ConvertDocument converts all pages of a document to markdown.
func ConvertDocument(doc *mupdf.Document) ([]PageResult, error) {
	nPages := doc.PageCount()

	// First pass: extract all pages to build the header map
	pages := make([]*mupdf.Page, nPages)
	for i := 0; i < nPages; i++ {
		p, err := doc.ExtractPage(i)
		if err != nil {
			pages[i] = &mupdf.Page{PageNum: i}
			continue
		}
		pages[i] = p
	}

	headers := BuildHeaderMap(pages, 12)

	// Second pass: convert each page to markdown
	results := make([]PageResult, nPages)
	for i, page := range pages {
		md := pageToMarkdown(page, headers)
		results[i] = PageResult{
			Markdown:  md,
			IsScanned: page.CharCount < scannedThreshold,
			PageNum:   i + 1,
		}
	}
	return results, nil
}

func pageToMarkdown(page *mupdf.Page, headers HeaderMap) string {
	lines := ReconstructLines(page, 3)
	if len(lines) == 0 {
		return ""
	}

	var out strings.Builder
	prevBlockNo := -1
	inCode := false
	prevHeaderPrefix := ""
	var prevBBox mupdf.Rect

	for _, vl := range lines {
		lineText := lineRawText(vl)
		if strings.TrimSpace(lineText) == "" {
			continue
		}

		allBold := allSpansHaveStyle(vl.Spans, func(s Span) bool { return s.Bold })
		allItalic := allSpansHaveStyle(vl.Spans, func(s Span) bool { return s.Italic })
		allMono := allSpansHaveStyle(vl.Spans, func(s Span) bool { return s.Mono })

		// Determine header prefix from font size
		headerPrefix := ""
		if len(vl.Spans) > 0 {
			headerPrefix = headers.GetHeaderPrefix(vl.Spans[0].Size)
		}

		// Header line
		if headerPrefix != "" {
			if inCode {
				out.WriteString("```\n")
				inCode = false
			}
			text := strings.TrimSpace(lineText)
			if allBold {
				text = "**" + text + "**"
			}
			if allItalic {
				text = "_" + text + "_"
			}
			// Multi-line header continuation
			if headerPrefix == prevHeaderPrefix && prevBBox.Y1 > 0 &&
				vl.BBox.Y0-prevBBox.Y1 < vl.BBox.Height()*0.5 {
				trimTrailingNewlines(&out)
				out.WriteString(" " + text + "\n")
			} else {
				out.WriteString("\n" + headerPrefix + text + "\n")
			}
			prevHeaderPrefix = headerPrefix
			prevBBox = vl.BBox
			prevBlockNo = vl.BlockNo
			continue
		}
		prevHeaderPrefix = ""

		// Code block (all mono-spaced)
		if allMono {
			if !inCode {
				out.WriteString("\n```\n")
				inCode = true
			}
			out.WriteString(lineText + "\n")
			prevBBox = vl.BBox
			prevBlockNo = vl.BlockNo
			continue
		}

		if inCode {
			out.WriteString("```\n\n")
			inCode = false
		}

		// Paragraph break detection
		if vl.BlockNo != prevBlockNo && prevBlockNo >= 0 {
			out.WriteString("\n")
		} else if prevBBox.Y1 > 0 && vl.BBox.Y0-prevBBox.Y1 > vl.BBox.Height()*0.5 {
			out.WriteString("\n")
		}

		// Check for bullet
		firstRune := firstNonSpaceRune(lineText)
		if IsBullet(firstRune) {
			charWidth := vl.BBox.Width() / float64(max(len(lineText), 1))
			formatted := FormatBulletLine(lineText, vl.BBox.X0, page.MediaBox.X0, charWidth)
			out.WriteString(formatted + "\n")
			prevBBox = vl.BBox
			prevBlockNo = vl.BlockNo
			continue
		}

		// Regular line with per-span formatting
		for i, sp := range vl.Spans {
			formatted := FormatSpan(sp)
			if formatted != "" {
				if i > 0 {
					out.WriteString(" ")
				}
				out.WriteString(formatted)
			}
		}
		out.WriteString("\n")

		prevBBox = vl.BBox
		prevBlockNo = vl.BlockNo
	}

	if inCode {
		out.WriteString("```\n")
	}

	result := out.String()
	result = strings.ReplaceAll(result, " \n", "\n")
	result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	return strings.TrimSpace(result)
}

func lineRawText(vl VisualLine) string {
	var buf strings.Builder
	for _, sp := range vl.Spans {
		buf.WriteString(sp.Text)
	}
	return buf.String()
}

func allSpansHaveStyle(spans []Span, check func(Span) bool) bool {
	if len(spans) == 0 {
		return false
	}
	for _, s := range spans {
		if strings.TrimSpace(s.Text) != "" && !check(s) {
			return false
		}
	}
	return true
}

func firstNonSpaceRune(s string) rune {
	for _, r := range s {
		if r != ' ' && r != '\t' {
			return r
		}
	}
	return 0
}

func trimTrailingNewlines(b *strings.Builder) {
	s := b.String()
	s = strings.TrimRight(s, "\n")
	b.Reset()
	b.WriteString(s)
}
