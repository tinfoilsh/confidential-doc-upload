package pdftomd

import (
	"math"
	"sort"

	"github.com/tinfoilsh/confidential-doc-upload/internal/mupdf"
)

// columnBoxes detects column layout on a page and returns bounding rectangles
// in reading order. Each rectangle wraps a column or section of text.
// Port of pymupdf4llm's column_boxes algorithm.
func columnBoxes(page *mupdf.Page, footerMargin, headerMargin float64) []mupdf.Rect {
	if footerMargin == 0 {
		footerMargin = 50
	}
	if headerMargin == 0 {
		headerMargin = 50
	}

	clip := page.MediaBox
	clip.Y1 -= footerMargin
	clip.Y0 += headerMargin

	// Collect text block bboxes (only horizontal text)
	var bboxes []mupdf.Rect
	for _, block := range page.Blocks {
		if block.Type != 0 || len(block.Lines) == 0 {
			continue
		}
		// Check first line is horizontal
		if math.Abs(1-block.Lines[0].Dir[0]) > 1e-3 {
			continue
		}
		// Compute tight bbox from non-empty lines
		var srect mupdf.Rect
		first := true
		for _, line := range block.Lines {
			hasText := false
			for _, ch := range line.Chars {
				if ch.Rune > ' ' {
					hasText = true
					break
				}
			}
			if hasText {
				if first {
					srect = line.BBox
					first = false
				} else {
					srect = rectUnion(srect, line.BBox)
				}
			}
		}
		if !first && !rectIsEmpty(srect) {
			// Skip blocks outside clip
			if srect.Y1 < clip.Y0 || srect.Y0 > clip.Y1 {
				continue
			}
			bboxes = append(bboxes, srect)
		}
	}

	if len(bboxes) == 0 {
		return nil
	}

	// Sort by y0 then x0
	sort.Slice(bboxes, func(i, j int) bool {
		if bboxes[i].Y0 != bboxes[j].Y0 {
			return bboxes[i].Y0 < bboxes[j].Y0
		}
		return bboxes[i].X0 < bboxes[j].X0
	})

	// Phase 1: merge blocks into columns
	nblocks := []mupdf.Rect{bboxes[0]}
	for _, bb := range bboxes[1:] {
		merged := false
		for j := range nblocks {
			nbb := nblocks[j]
			// Don't join across columns (no horizontal overlap)
			if nbb.X1 < bb.X0 || bb.X1 < nbb.X0 {
				continue
			}
			// Try extending
			temp := rectUnion(bb, nbb)
			if canExtend(temp, nbb, nblocks) {
				nblocks[j] = temp
				merged = true
				break
			}
		}
		if !merged {
			nblocks = append(nblocks, bb)
		}
	}

	// Clean: remove duplicates, sort same-bottom blocks left to right
	nblocks = cleanBlocks(nblocks)

	// Phase 2: snap edges and merge vertically adjacent blocks
	nblocks = joinPhase2(nblocks)

	// Phase 3: merge compatible blocks + reading order sort
	nblocks = joinPhase3(nblocks)

	return nblocks
}

func canExtend(temp, self mupdf.Rect, others []mupdf.Rect) bool {
	for _, o := range others {
		if rectsEqual(o, self) {
			continue
		}
		if !rectIsEmpty(rectIntersect(temp, o)) {
			return false
		}
	}
	return true
}

func cleanBlocks(blocks []mupdf.Rect) []mupdf.Rect {
	if len(blocks) < 2 {
		return blocks
	}
	// Remove duplicates
	var cleaned []mupdf.Rect
	for i, b := range blocks {
		if i > 0 && rectsEqual(b, blocks[i-1]) {
			continue
		}
		cleaned = append(cleaned, b)
	}
	// Sort segments with same bottom left-to-right
	sortSameBottom(cleaned, 3)
	return cleaned
}

func sortSameBottom(blocks []mupdf.Rect, tolerance float64) {
	if len(blocks) < 2 {
		return
	}
	i0 := 0
	y1 := blocks[0].Y1
	for i := 1; i < len(blocks); i++ {
		if math.Abs(blocks[i].Y1-y1) > tolerance {
			if i-i0 > 1 {
				sort.Slice(blocks[i0:i], func(a, b int) bool {
					return blocks[i0+a].X0 < blocks[i0+b].X0
				})
			}
			y1 = blocks[i].Y1
			i0 = i
		}
	}
	if len(blocks)-i0 > 1 {
		sort.Slice(blocks[i0:], func(a, b int) bool {
			return blocks[i0+a].X0 < blocks[i0+b].X0
		})
	}
}

func joinPhase2(blocks []mupdf.Rect) []mupdf.Rect {
	if len(blocks) < 2 {
		return blocks
	}
	// Snap edges: align x0/x1 within 3pt
	for i := range blocks {
		b := blocks[i]
		minX0 := b.X0
		maxX1 := b.X1
		for _, bb := range blocks {
			if math.Abs(bb.X0-b.X0) <= 3 && bb.X0 < minX0 {
				minX0 = bb.X0
			}
			if math.Abs(bb.X1-b.X1) <= 3 && bb.X1 > maxX1 {
				maxX1 = bb.X1
			}
		}
		blocks[i].X0 = minX0
		blocks[i].X1 = maxX1
	}

	sort.Slice(blocks, func(i, j int) bool {
		if blocks[i].X0 != blocks[j].X0 {
			return blocks[i].X0 < blocks[j].X0
		}
		return blocks[i].Y0 < blocks[j].Y0
	})

	result := []mupdf.Rect{blocks[0]}
	for _, r := range blocks[1:] {
		r0 := result[len(result)-1]
		if math.Abs(r.X0-r0.X0) <= 3 &&
			math.Abs(r.X1-r0.X1) <= 3 &&
			math.Abs(r0.Y1-r.Y0) <= 10 {
			result[len(result)-1] = rectUnion(r0, r)
		} else {
			result = append(result, r)
		}
	}
	return result
}

func joinPhase3(blocks []mupdf.Rect) []mupdf.Rect {
	if len(blocks) < 2 {
		return blocks
	}

	// Merge compatible blocks
	prects := make([]mupdf.Rect, len(blocks))
	copy(prects, blocks)

	var result []mupdf.Rect
	for len(prects) > 0 {
		r0 := prects[0]
		changed := true
		for changed {
			changed = false
			for i := len(prects) - 1; i > 0; i-- {
				r1 := prects[i]
				// Don't join across columns
				if r1.X0 > r0.X1 || r1.X1 < r0.X0 {
					continue
				}
				temp := rectUnion(r0, r1)
				// Check union doesn't engulf other rects
				touching := 0
				for _, pr := range append(prects, result...) {
					if !rectIsEmpty(rectIntersect(temp, pr)) {
						touching++
					}
				}
				if touching <= 2 {
					r0 = temp
					prects[0] = r0
					prects = append(prects[:i], prects[i+1:]...)
					changed = true
				}
			}
		}
		result = append(result, r0)
		prects = prects[1:]
	}

	// Reading order: P-Q sort trick
	type sortEntry struct {
		rect mupdf.Rect
		key  [2]float64
	}
	entries := make([]sortEntry, len(result))
	for i, box := range result {
		// Find left-most rect that vertically overlaps this one
		key := [2]float64{box.Y0, box.X0}
		for _, r := range result {
			if r.X1 < box.X0 &&
				(box.Y0 <= r.Y0 && r.Y0 <= box.Y1 || box.Y0 <= r.Y1 && r.Y1 <= box.Y1) {
				if r.Y0 < key[0] || (r.Y0 == key[0] && r.X0 < key[1]) {
					key = [2]float64{r.Y0, box.X0}
				}
			}
		}
		entries[i] = sortEntry{rect: box, key: key}
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].key[0] != entries[j].key[0] {
			return entries[i].key[0] < entries[j].key[0]
		}
		return entries[i].key[1] < entries[j].key[1]
	})

	sorted := make([]mupdf.Rect, len(entries))
	for i, e := range entries {
		sorted[i] = e.rect
	}
	return sorted
}

// Rect helpers

func rectUnion(a, b mupdf.Rect) mupdf.Rect {
	return mupdf.Rect{
		X0: math.Min(a.X0, b.X0),
		Y0: math.Min(a.Y0, b.Y0),
		X1: math.Max(a.X1, b.X1),
		Y1: math.Max(a.Y1, b.Y1),
	}
}

func rectIntersect(a, b mupdf.Rect) mupdf.Rect {
	r := mupdf.Rect{
		X0: math.Max(a.X0, b.X0),
		Y0: math.Max(a.Y0, b.Y0),
		X1: math.Min(a.X1, b.X1),
		Y1: math.Min(a.Y1, b.Y1),
	}
	if r.X0 >= r.X1 || r.Y0 >= r.Y1 {
		return mupdf.Rect{}
	}
	return r
}

func rectIsEmpty(r mupdf.Rect) bool {
	return r.X0 >= r.X1 || r.Y0 >= r.Y1
}

func rectsEqual(a, b mupdf.Rect) bool {
	return a.X0 == b.X0 && a.Y0 == b.Y0 && a.X1 == b.X1 && a.Y1 == b.Y1
}

func rectContainsPoint(r mupdf.Rect, x, y float64) bool {
	return x >= r.X0 && x <= r.X1 && y >= r.Y0 && y <= r.Y1
}
