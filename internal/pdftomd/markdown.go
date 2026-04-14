package pdftomd

import (
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
			pages[i] = &mupdf.Page{PageNum: i}
			continue
		}
		pages[i] = p
	}

	headers := BuildHeaderMap(pages, 12)

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
	columns := columnBoxes(page, 0, 0)

	// If column detection found regions, extract per column
	if len(columns) > 0 {
		var parts []string
		for _, col := range columns {
			md := columnToMarkdown(page, headers, col)
			if md != "" {
				parts = append(parts, md)
			}
		}
		result := strings.Join(parts, "\n\n")
		return strings.TrimSpace(result)
	}

	// Fallback: process whole page
	return columnToMarkdown(page, headers, page.MediaBox)
}

func columnToMarkdown(page *mupdf.Page, headers HeaderMap, clip mupdf.Rect) string {
	lines := ReconstructLinesInClip(page, 3, clip)
	if len(lines) == 0 {
		return ""
	}

	var out strings.Builder
	prevBlockNo := -1
	inCode := false
	prevHeaderPrefix := ""
	var prevBBox mupdf.Rect
	prevWasJoinable := false

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

		firstRune := firstNonSpaceRune(lineText)
		isBullet := IsBullet(firstRune)

		// Determine if this line continues the previous paragraph
		// (same block, small y-gap, not a header/bullet/code, not starting a new section)
		isJoinable := headerPrefix == "" && !allMono && !isBullet &&
			vl.BlockNo == prevBlockNo && prevBBox.Y1 > 0 &&
			vl.BBox.Y0-prevBBox.Y1 <= vl.BBox.Height()*0.3

		// Header line
		if headerPrefix != "" {
			if inCode {
				out.WriteString("```\n")
				inCode = false
			}
			if prevWasJoinable {
				out.WriteString("\n")
			}
			text := strings.TrimSpace(lineText)
			if allBold {
				text = "**" + text + "**"
			}
			if allItalic {
				text = "_" + text + "_"
			}
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
			prevWasJoinable = false
			continue
		}
		prevHeaderPrefix = ""

		// Code block (all mono-spaced)
		if allMono {
			if prevWasJoinable {
				out.WriteString("\n")
			}
			if !inCode {
				out.WriteString("\n```\n")
				inCode = true
			}
			out.WriteString(lineText + "\n")
			prevBBox = vl.BBox
			prevBlockNo = vl.BlockNo
			prevWasJoinable = false
			continue
		}

		if inCode {
			out.WriteString("```\n\n")
			inCode = false
		}

		// Join continuation lines within the same paragraph
		if isJoinable && prevWasJoinable {
			text := strings.TrimSpace(lineText)
			// Emit with per-span formatting for the continuation
			out.WriteString(" ")
			writeFormattedSpans(&out, vl.Spans)
			prevBBox = vl.BBox
			prevBlockNo = vl.BlockNo
			prevWasJoinable = true
			_ = text
			continue
		}

		// Paragraph break
		if prevBlockNo >= 0 {
			if prevWasJoinable {
				out.WriteString("\n")
			}
			if vl.BlockNo != prevBlockNo {
				out.WriteString("\n")
			} else if prevBBox.Y1 > 0 && vl.BBox.Y0-prevBBox.Y1 > vl.BBox.Height()*0.5 {
				out.WriteString("\n")
			}
		}

		// Bullet
		if isBullet {
			charWidth := vl.BBox.Width() / float64(max(len(lineText), 1))
			formatted := FormatBulletLine(lineText, vl.BBox.X0, page.MediaBox.X0, charWidth)
			out.WriteString(formatted + "\n")
			prevBBox = vl.BBox
			prevBlockNo = vl.BlockNo
			prevWasJoinable = false
			continue
		}

		// Regular line - emit with per-span formatting
		writeFormattedSpans(&out, vl.Spans)
		prevBBox = vl.BBox
		prevBlockNo = vl.BlockNo
		prevWasJoinable = !isBullet && headerPrefix == "" && !allMono
	}

	if prevWasJoinable {
		out.WriteString("\n")
	}
	if inCode {
		out.WriteString("```\n")
	}

	result := out.String()
	result = strings.ReplaceAll(result, " \n", "\n")
	result = strings.ReplaceAll(result, "\n\n\n", "\n\n")
	return strings.TrimSpace(result)
}

func writeFormattedSpans(out *strings.Builder, spans []Span) {
	for i, sp := range spans {
		formatted := FormatSpan(sp)
		if formatted != "" {
			if i > 0 {
				out.WriteString(" ")
			}
			out.WriteString(formatted)
		}
	}
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
