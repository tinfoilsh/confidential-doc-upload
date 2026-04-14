package pdftomd

import (
	"log/slog"
	"strings"

	"github.com/tinfoilsh/confidential-doc-upload/internal/mupdf"
)

const scannedThreshold = 50

type PageResult struct {
	Markdown  string `json:"md_content"`
	IsScanned bool   `json:"is_scanned"`
	PageNum   int    `json:"page"`
}

func ConvertDocument(doc *mupdf.Document) ([]PageResult, error) {
	nPages := doc.PageCount()

	pages := make([]*mupdf.Page, nPages)
	for i := 0; i < nPages; i++ {
		p, err := doc.ExtractPage(i)
		if err != nil {
			slog.Warn("failed to extract page, skipping", "page", i, "err", err)
			pages[i] = &mupdf.Page{PageNum: i}
			continue
		}
		pages[i] = p
	}

	headers := BuildHeaderMap(pages, 12)

	results := make([]PageResult, nPages)
	for i, page := range pages {
		md := pageToMarkdown(page, headers, page.LineSegments)
		results[i] = PageResult{
			Markdown:  md,
			IsScanned: page.CharCount < scannedThreshold,
			PageNum:   i + 1,
		}
	}
	return results, nil
}

func pageToMarkdown(page *mupdf.Page, headers HeaderMap, segments []mupdf.LineSegment) string {
	// Detect tables from vector line segments (bordered tables)
	tables := DetectTables(page, segments)

	// Also detect text-aligned tables with horizontal rules only
	textTables := DetectTextTables(page, segments)

	// Merge, deduplicating by bbox overlap
	for _, tt := range textTables {
		overlaps := false
		for _, t := range tables {
			ix := rectIntersect(t.BBox, tt.BBox)
			if !rectIsEmpty(ix) {
				area := ix.Width() * ix.Height()
				ttArea := tt.BBox.Width() * tt.BBox.Height()
				if ttArea > 0 && area/ttArea > 0.3 {
					overlaps = true
					break
				}
			}
		}
		if !overlaps {
			tables = append(tables, tt)
		}
	}

	columns := columnBoxes(page, 0, 0)

	if len(columns) > 0 {
		var parts []string
		for _, col := range columns {
			md := columnToMarkdown(page, headers, col, tables)
			if md != "" {
				parts = append(parts, md)
			}
		}
		result := strings.Join(parts, "\n\n")
		return strings.TrimSpace(result)
	}

	return columnToMarkdown(page, headers, page.MediaBox, tables)
}

// columnToMarkdown converts text within a clip region to markdown.
// Follows pymupdf4llm's write_text logic:
// - Each line gets \n at the end
// - Block change adds \n (paragraph break)
// - Y-gap > 1.5x line height adds \n
// - Bullets and bracket-starts add \n
// - Superscript starts add \n
func columnToMarkdown(page *mupdf.Page, headers HeaderMap, clip mupdf.Rect, tables []TableResult) string {
	lines := ReconstructLinesInClip(page, 3, clip)
	if len(lines) == 0 && len(tables) == 0 {
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

		headerPrefix := ""
		if len(vl.Spans) > 0 {
			headerPrefix = headers.GetHeaderPrefix(vl.Spans[0].Size)
		}

		// === Header line ===
		if headerPrefix != "" {
			if inCode {
				out.WriteString("```\n")
				inCode = false
			}
			text := strings.TrimSpace(lineText)
			if allMono {
				text = "`" + text + "`"
			}
			if allItalic {
				text = "_" + text + "_"
			}
			if allBold {
				text = "**" + text + "**"
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

		// === Code block (all mono-spaced) ===
		if allMono {
			if !inCode {
				out.WriteString("```\n")
				inCode = true
			}
			// Approximate indentation from x-offset
			if len(vl.Spans) > 0 && vl.Spans[0].Size > 0 {
				delta := int((vl.BBox.X0 - clip.X0) / (vl.Spans[0].Size * 0.5))
				if delta > 0 {
					out.WriteString(strings.Repeat(" ", delta))
				}
			}
			out.WriteString(strings.TrimSpace(lineText) + "\n")
			prevBBox = vl.BBox
			prevBlockNo = vl.BlockNo
			continue
		}

		if inCode {
			out.WriteString("```\n")
			inCode = false
		}

		// === Paragraph break detection (matches pymupdf4llm) ===
		// Block number change = new paragraph
		if vl.BlockNo != prevBlockNo && prevBlockNo >= 0 {
			out.WriteString("\n")
			prevBlockNo = vl.BlockNo
		}

		// Additional line break conditions
		firstSpan := vl.Spans[0]
		needBreak := false
		if prevBBox.Y1 > 0 && vl.BBox.Y1-prevBBox.Y1 > vl.BBox.Height()*1.5 {
			needBreak = true
		}
		if strings.HasPrefix(firstSpan.Text, "[") {
			needBreak = true
		}
		if IsBullet(firstNonSpaceRune(lineText)) {
			needBreak = true
		}
		if firstSpan.Superscript {
			needBreak = true
		}
		if needBreak {
			out.WriteString("\n")
		}
		prevBBox = vl.BBox

		// === Bullet handling ===
		firstRune := firstNonSpaceRune(lineText)
		if IsBullet(firstRune) {
			charWidth := vl.BBox.Width() / float64(max(len(lineText), 1))
			formatted := FormatBulletLine(lineText, vl.BBox.X0, clip.X0, charWidth)
			out.WriteString(formatted + "\n")
			prevBlockNo = vl.BlockNo
			continue
		}

		// === Regular text: format each span ===
		for _, sp := range vl.Spans {
			formatted := FormatSpan(sp)
			if formatted != "" {
				out.WriteString(formatted + " ")
			}
		}
		out.WriteString("\n")
		prevBlockNo = vl.BlockNo
	}

	if inCode {
		out.WriteString("```\n")
	}

	// Append any detected tables in this column
	for _, t := range tables {
		centerX := (t.BBox.X0 + t.BBox.X1) / 2
		centerY := (t.BBox.Y0 + t.BBox.Y1) / 2
		if centerX >= clip.X0 && centerX <= clip.X1 && centerY >= clip.Y0 && centerY <= clip.Y1 {
			out.WriteString("\n" + t.Markdown + "\n")
		}
	}

	result := out.String()
	result = strings.ReplaceAll(result, " \n", "\n")
	result = strings.ReplaceAll(result, "  ", " ")
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
