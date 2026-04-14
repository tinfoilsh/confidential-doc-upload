package pdftomd

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/tinfoilsh/confidential-doc-upload/internal/mupdf"
)

// DetectTextTables finds tables that have horizontal rules but no vertical borders.
// It infers column positions from text alignment between the horizontal lines.
func DetectTextTables(page *mupdf.Page, segments []mupdf.LineSegment) []TableResult {
	// Find horizontal line clusters (groups of H lines at similar x-ranges)
	var hLines []edge
	for _, s := range segments {
		if s.IsHorizontal != 1 {
			continue
		}
		e := edge{
			x0:          math.Min(s.X0, s.X1),
			y0:          (s.Y0 + s.Y1) / 2,
			x1:          math.Max(s.X0, s.X1),
			y1:          (s.Y0 + s.Y1) / 2,
			orientation: "h",
		}
		if edgeLength(e) < 50 { // skip short lines
			continue
		}
		hLines = append(hLines, e)
	}

	if len(hLines) < 2 {
		return nil
	}

	// Snap horizontal lines by y-position
	hLines = snapByAttr(hLines, func(e edge) float64 { return e.y0 }, 3, true)

	// Group H lines by similar x-range (same table)
	sort.Slice(hLines, func(i, j int) bool { return hLines[i].y0 < hLines[j].y0 })

	var tableGroups [][]edge
	for _, h := range hLines {
		placed := false
		for gi := range tableGroups {
			g := tableGroups[gi]
			// Check if x-ranges overlap significantly
			ref := g[0]
			overlapX := math.Min(h.x1, ref.x1) - math.Max(h.x0, ref.x0)
			refWidth := ref.x1 - ref.x0
			if refWidth > 0 && overlapX/refWidth > 0.5 {
				tableGroups[gi] = append(tableGroups[gi], h)
				placed = true
				break
			}
		}
		if !placed {
			tableGroups = append(tableGroups, []edge{h})
		}
	}

	var results []TableResult
	for _, group := range tableGroups {
		if len(group) < 2 {
			continue
		}

		// Get the table bbox from the line group
		sort.Slice(group, func(i, j int) bool { return group[i].y0 < group[j].y0 })
		tableBbox := mupdf.Rect{
			X0: group[0].x0, Y0: group[0].y0,
			X1: group[0].x1, Y1: group[len(group)-1].y0,
		}
		for _, h := range group {
			tableBbox.X0 = math.Min(tableBbox.X0, h.x0)
			tableBbox.X1 = math.Max(tableBbox.X1, h.x1)
		}

		// Check if any bordered table already covers this region (skip if so)
		// We handle this in the caller by deduplication.

		// Collect row boundaries from H lines
		var rowYs []float64
		for _, h := range group {
			rowYs = appendUniqueFloat(rowYs, h.y0, 2)
		}
		sort.Float64s(rowYs)

		if len(rowYs) < 2 {
			continue
		}

		// Collect all text chars within the table region
		type charInfo struct {
			r    rune
			x, y float64
		}
		var chars []charInfo
		for _, block := range page.Blocks {
			if block.Type != 0 {
				continue
			}
			for _, line := range block.Lines {
				for _, ch := range line.Chars {
					if ch.Rune <= ' ' {
						continue
					}
					cx, cy := ch.Origin[0], ch.Origin[1]
					if cx >= tableBbox.X0-5 && cx <= tableBbox.X1+5 &&
						cy >= tableBbox.Y0-5 && cy <= tableBbox.Y1+5 {
						chars = append(chars, charInfo{r: ch.Rune, x: cx, y: cy})
					}
				}
			}
		}

		if len(chars) == 0 {
			continue
		}

		// Infer column boundaries from text x-position gaps
		sort.Slice(chars, func(i, j int) bool { return chars[i].x < chars[j].x })
		var xPositions []float64
		for _, c := range chars {
			xPositions = append(xPositions, c.x)
		}

		colXs := inferColumnBoundaries(xPositions, tableBbox.X0, tableBbox.X1)
		if len(colXs) < 2 {
			continue
		}

		nRows := len(rowYs) - 1
		nCols := len(colXs) - 1
		if nRows < 1 || nCols < 1 {
			continue
		}

		// Map chars into grid
		grid := make([][]string, nRows)
		for r := range grid {
			grid[r] = make([]string, nCols)
		}

		for _, block := range page.Blocks {
			if block.Type != 0 {
				continue
			}
			for _, line := range block.Lines {
				for _, ch := range line.Chars {
					cx, cy := ch.Origin[0], ch.Origin[1]
					col := findIdx(colXs, cx)
					row := findIdx(rowYs, cy)
					if row >= 0 && row < nRows && col >= 0 && col < nCols {
						if ch.Rune == ' ' {
							if len(grid[row][col]) > 0 && grid[row][col][len(grid[row][col])-1] != ' ' {
								grid[row][col] += " "
							}
						} else {
							grid[row][col] += string(ch.Rune)
						}
					}
				}
			}
		}

		// Clean and check content
		filledCells := 0
		for r := range grid {
			for c := range grid[r] {
				grid[r][c] = strings.TrimSpace(grid[r][c])
				if grid[r][c] != "" {
					filledCells++
				}
			}
		}
		totalCells := nRows * nCols
		if filledCells == 0 || float64(filledCells)/float64(totalCells) < 0.2 {
			continue
		}

		// Render markdown
		var out strings.Builder
		for r, row := range grid {
			out.WriteString("|")
			for _, cell := range row {
				text := strings.ReplaceAll(cell, "|", "\\|")
				out.WriteString(fmt.Sprintf("%s|", text))
			}
			out.WriteString("\n")
			if r == 0 {
				out.WriteString("|")
				for range row {
					out.WriteString("---|")
				}
				out.WriteString("\n")
			}
		}

		md := out.String()
		if md != "" {
			results = append(results, TableResult{Markdown: md, BBox: tableBbox})
		}
	}

	return results
}

// inferColumnBoundaries finds column edges from x-position gaps.
func inferColumnBoundaries(xPositions []float64, tableLeft, tableRight float64) []float64 {
	if len(xPositions) == 0 {
		return nil
	}

	// Find gaps in x-positions that indicate column boundaries
	sort.Float64s(xPositions)

	// Compute gaps between consecutive x-positions
	type gap struct {
		pos  float64
		size float64
	}
	var gaps []gap
	for i := 1; i < len(xPositions); i++ {
		g := xPositions[i] - xPositions[i-1]
		if g > 10 { // minimum gap to be a column boundary
			gaps = append(gaps, gap{pos: (xPositions[i-1] + xPositions[i]) / 2, size: g})
		}
	}

	// Sort gaps by size, take the largest ones as column boundaries
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].size > gaps[j].size })

	// Collect boundary positions
	boundaries := []float64{tableLeft}
	for _, g := range gaps {
		if g.size > 15 { // significant gap
			boundaries = append(boundaries, g.pos)
		}
	}
	boundaries = append(boundaries, tableRight)
	sort.Float64s(boundaries)

	return boundaries
}

func findIdx(positions []float64, val float64) int {
	for i := 0; i < len(positions)-1; i++ {
		if val >= positions[i]-2 && val < positions[i+1]+2 {
			return i
		}
	}
	return -1
}

func appendUniqueFloat(s []float64, v float64, tol float64) []float64 {
	for _, x := range s {
		if math.Abs(x-v) < tol {
			return s
		}
	}
	return append(s, v)
}
