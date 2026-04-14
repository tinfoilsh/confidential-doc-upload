package pdftomd

import (
	"math"
	"sort"
	"strings"

	"github.com/tinfoilsh/confidential-doc-upload/internal/mupdf"
)

// VisualLine is a reconstructed line of text from possibly multiple MuPDF lines
// that share a visual baseline.
type VisualLine struct {
	BBox    mupdf.Rect
	Spans   []Span
	BlockNo int
}

// ReconstructLines groups MuPDF lines into visual lines based on y-proximity,
// then builds spans with style info from the character data.
func ReconstructLines(page *mupdf.Page, tolerance float64) []VisualLine {
	if tolerance == 0 {
		tolerance = 3
	}

	type rawSpan struct {
		chars   []mupdf.TextChar
		bbox    mupdf.Rect
		blockNo int
	}

	var allSpans []rawSpan
	for bi, block := range page.Blocks {
		if block.Type != 0 {
			continue
		}
		for _, line := range block.Lines {
			if line.WMode != 0 {
				continue
			}
			if len(line.Chars) == 0 {
				continue
			}
			allSpans = append(allSpans, rawSpan{
				chars:   line.Chars,
				bbox:    line.BBox,
				blockNo: bi,
			})
		}
	}

	sort.Slice(allSpans, func(i, j int) bool {
		return allSpans[i].bbox.Y1 < allSpans[j].bbox.Y1
	})

	// Group into visual lines by y-proximity
	var groups [][]rawSpan
	for _, sp := range allSpans {
		if len(groups) == 0 {
			groups = append(groups, []rawSpan{sp})
			continue
		}
		last := groups[len(groups)-1]
		lastBottom := last[len(last)-1].bbox.Y1
		if sp.bbox.Y1-lastBottom <= tolerance {
			groups[len(groups)-1] = append(groups[len(groups)-1], sp)
		} else {
			groups = append(groups, []rawSpan{sp})
		}
	}

	var result []VisualLine
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			return group[i].bbox.X0 < group[j].bbox.X0
		})

		// Deduplicate overlapping spans (same position = same text rendered twice)
		var deduped []rawSpan
		for _, sp := range group {
			isDupe := false
			for _, existing := range deduped {
				if math.Abs(sp.bbox.X0-existing.bbox.X0) < 1 &&
					math.Abs(sp.bbox.Y0-existing.bbox.Y0) < 1 &&
					len(sp.chars) > 0 && len(existing.chars) > 0 &&
					sp.chars[0].Rune == existing.chars[0].Rune {
					isDupe = true
					break
				}
			}
			if !isDupe {
				deduped = append(deduped, sp)
			}
		}

		vl := VisualLine{BlockNo: deduped[0].blockNo}
		var allChars []mupdf.TextChar
		for _, sp := range deduped {
			allChars = append(allChars, sp.chars...)
			vl.BBox = unionRect(vl.BBox, sp.bbox)
		}

		vl.Spans = buildSpans(allChars)
		result = append(result, vl)
	}

	return result
}

// buildSpans groups consecutive characters with the same style into Spans.
func buildSpans(chars []mupdf.TextChar) []Span {
	if len(chars) == 0 {
		return nil
	}

	var spans []Span
	var cur Span
	cur.Bold = chars[0].Bold
	cur.Italic = chars[0].Italic
	cur.Mono = chars[0].Mono
	cur.Size = chars[0].Size
	cur.X0 = chars[0].Origin[0]

	var buf strings.Builder
	for _, ch := range chars {
		sameStyle := ch.Bold == cur.Bold && ch.Italic == cur.Italic && ch.Mono == cur.Mono
		if !sameStyle {
			cur.Text = buf.String()
			if cur.Text != "" {
				spans = append(spans, cur)
			}
			buf.Reset()
			cur = Span{
				Bold:   ch.Bold,
				Italic: ch.Italic,
				Mono:   ch.Mono,
				Size:   ch.Size,
				X0:     ch.Origin[0],
			}
		}
		buf.WriteRune(ch.Rune)
	}
	cur.Text = buf.String()
	if cur.Text != "" {
		spans = append(spans, cur)
	}
	return spans
}

func unionRect(a, b mupdf.Rect) mupdf.Rect {
	if a.X0 == 0 && a.Y0 == 0 && a.X1 == 0 && a.Y1 == 0 {
		return b
	}
	return mupdf.Rect{
		X0: min(a.X0, b.X0),
		Y0: min(a.Y0, b.Y0),
		X1: max(a.X1, b.X1),
		Y1: max(a.Y1, b.Y1),
	}
}
