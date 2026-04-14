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

// ReconstructLines groups MuPDF lines into visual lines for the whole page.
func ReconstructLines(page *mupdf.Page, tolerance float64) []VisualLine {
	return ReconstructLinesInClip(page, tolerance, page.MediaBox)
}

// ReconstructLinesInClip groups MuPDF lines into visual lines within a clip rectangle.
// Lines are only merged if they come from blocks with overlapping x-ranges
// (same column), preventing cross-column merging.
func ReconstructLinesInClip(page *mupdf.Page, tolerance float64, clip mupdf.Rect) []VisualLine {
	if tolerance == 0 {
		tolerance = 3
	}

	type rawSpan struct {
		chars    []mupdf.TextChar
		bbox     mupdf.Rect
		blockNo  int
		blockBox mupdf.Rect
	}

	var allSpans []rawSpan
	for bi, block := range page.Blocks {
		if block.Type != 0 {
			continue
		}
		for _, line := range block.Lines {
			if line.WMode != 0 || len(line.Chars) == 0 {
				continue
			}
			lineCenterX := (line.BBox.X0 + line.BBox.X1) / 2
			lineCenterY := (line.BBox.Y0 + line.BBox.Y1) / 2
			if lineCenterX < clip.X0 || lineCenterX > clip.X1 ||
				lineCenterY < clip.Y0 || lineCenterY > clip.Y1 {
				continue
			}
			allSpans = append(allSpans, rawSpan{
				chars:    line.Chars,
				bbox:     line.BBox,
				blockNo:  bi,
				blockBox: block.BBox,
			})
		}
	}

	// Sort by block reading order (preserve MuPDF's block sequence),
	// then by Y within each block
	sort.SliceStable(allSpans, func(i, j int) bool {
		bi, bj := allSpans[i].blockNo, allSpans[j].blockNo
		if bi != bj {
			return bi < bj
		}
		return allSpans[i].bbox.Y0 < allSpans[j].bbox.Y0
	})

	// Group into visual lines, but only merge spans from blocks that
	// horizontally overlap (same column)
	var groups [][]rawSpan
	for _, sp := range allSpans {
		merged := false
		if len(groups) > 0 {
			last := groups[len(groups)-1]
			lastSpan := last[len(last)-1]
			sameY := math.Abs(sp.bbox.Y1-lastSpan.bbox.Y1) <= tolerance
			// Check horizontal overlap between blocks
			sameColumn := blocksOverlapX(sp.blockBox, lastSpan.blockBox)
			if sameY && sameColumn {
				groups[len(groups)-1] = append(groups[len(groups)-1], sp)
				merged = true
			}
		}
		if !merged {
			groups = append(groups, []rawSpan{sp})
		}
	}

	var result []VisualLine
	for _, group := range groups {
		sort.Slice(group, func(i, j int) bool {
			return group[i].bbox.X0 < group[j].bbox.X0
		})

		// Deduplicate overlapping spans
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

// blocksOverlapX checks if two block bboxes have significant horizontal overlap.
func blocksOverlapX(a, b mupdf.Rect) bool {
	overlap := math.Min(a.X1, b.X1) - math.Max(a.X0, b.X0)
	minWidth := math.Min(a.X1-a.X0, b.X1-b.X0)
	if minWidth <= 0 {
		return true
	}
	return overlap/minWidth > 0.3
}

// buildSpans groups consecutive characters with the same style into Spans.
// Also detects superscripts heuristically (smaller size + higher baseline).
func buildSpans(chars []mupdf.TextChar) []Span {
	if len(chars) == 0 {
		return nil
	}

	// Compute median font size for superscript detection
	medianSize := medianFontSize(chars)

	var spans []Span
	var cur Span
	cur.Bold = chars[0].Bold
	cur.Italic = chars[0].Italic
	cur.Mono = chars[0].Mono
	cur.Strikeout = chars[0].Strikeout
	cur.Superscript = isSuperscript(chars[0], medianSize)
	cur.Size = chars[0].Size
	cur.X0 = chars[0].Origin[0]

	var buf strings.Builder
	for _, ch := range chars {
		isSuper := isSuperscript(ch, medianSize)
		sameStyle := ch.Bold == cur.Bold && ch.Italic == cur.Italic &&
			ch.Mono == cur.Mono && ch.Strikeout == cur.Strikeout &&
			isSuper == cur.Superscript
		if !sameStyle {
			cur.Text = buf.String()
			if cur.Text != "" {
				spans = append(spans, cur)
			}
			buf.Reset()
			cur = Span{
				Bold:        ch.Bold,
				Italic:      ch.Italic,
				Mono:        ch.Mono,
				Strikeout:   ch.Strikeout,
				Superscript: isSuper,
				Size:        ch.Size,
				X0:          ch.Origin[0],
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

func isSuperscript(ch mupdf.TextChar, medianSize float64) bool {
	return medianSize > 0 && ch.Size < medianSize*0.75
}

func medianFontSize(chars []mupdf.TextChar) float64 {
	if len(chars) == 0 {
		return 0
	}
	sizes := make([]float64, len(chars))
	for i, ch := range chars {
		sizes[i] = ch.Size
	}
	sort.Float64s(sizes)
	return sizes[len(sizes)/2]
}

func unionRect(a, b mupdf.Rect) mupdf.Rect {
	if a.X0 == 0 && a.Y0 == 0 && a.X1 == 0 && a.Y1 == 0 {
		return b
	}
	return rectUnion(a, b)
}
