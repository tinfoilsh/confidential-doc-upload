package pdftomd

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/tinfoilsh/confidential-doc-upload/internal/mupdf"
)

const (
	snapTolerance         = 3.0
	joinTolerance         = 3.0
	edgeMinLength         = 3.0
	intersectionTolerance = 3.0
)

type edge struct {
	x0, y0, x1, y1 float64
	orientation     string // "h" or "v"
}

type intersection struct {
	x, y float64
	vEdges, hEdges []int // indices into edges slice
}

// DetectTables finds tables on a page using vector path line segments.
// Returns markdown table strings positioned by their bbox.
func DetectTables(page *mupdf.Page, segments []mupdf.LineSegment) []TableResult {
	// 1. Convert segments to edges (only axis-parallel lines)
	edges := segmentsToEdges(segments)
	if len(edges) == 0 {
		return nil
	}

	// 2. Snap and merge edges
	edges = mergeEdges(edges)

	// 3. Filter short edges
	var filtered []edge
	for _, e := range edges {
		l := edgeLength(e)
		if l >= edgeMinLength {
			filtered = append(filtered, e)
		}
	}
	edges = filtered
	if len(edges) == 0 {
		return nil
	}

	// 4. Find intersections
	intersections := findIntersections(edges)
	if len(intersections) < 4 {
		return nil
	}

	// 5. Build cells from intersections
	cells := buildCells(intersections, edges)
	if len(cells) == 0 {
		return nil
	}

	// 6. Group cells into tables
	tables := groupCellsIntoTables(cells)

	// 7. Render each table to markdown, filtering out false positives
	var results []TableResult
	for _, tableCells := range tables {
		// Require at least 2x2 grid (4 cells minimum)
		if len(tableCells) < 4 {
			continue
		}
		md := renderTableMarkdown(tableCells, page)
		if md != "" {
			bbox := tableBBox(tableCells)
			results = append(results, TableResult{Markdown: md, BBox: bbox})
		}
	}
	return results
}

type TableResult struct {
	Markdown string
	BBox     mupdf.Rect
}

type cell struct {
	x0, y0, x1, y1 float64
}

func segmentsToEdges(segments []mupdf.LineSegment) []edge {
	var edges []edge
	for _, s := range segments {
		if s.IsHorizontal == -1 {
			continue // skip diagonal lines
		}
		e := edge{
			x0: math.Min(s.X0, s.X1), y0: math.Min(s.Y0, s.Y1),
			x1: math.Max(s.X0, s.X1), y1: math.Max(s.Y0, s.Y1),
		}
		if s.IsHorizontal == 1 {
			e.orientation = "h"
			e.y0 = (s.Y0 + s.Y1) / 2
			e.y1 = e.y0
		} else {
			e.orientation = "v"
			e.x0 = (s.X0 + s.X1) / 2
			e.x1 = e.x0
		}
		edges = append(edges, e)
	}
	return edges
}

func mergeEdges(edges []edge) []edge {
	edges = snapEdges(edges)
	edges = joinEdges(edges)
	return edges
}

// snapEdges clusters nearby parallel edges and moves them to the average position.
func snapEdges(edges []edge) []edge {
	var hEdges, vEdges []edge
	for _, e := range edges {
		if e.orientation == "h" {
			hEdges = append(hEdges, e)
		} else {
			vEdges = append(vEdges, e)
		}
	}
	hEdges = snapByAttr(hEdges, func(e edge) float64 { return e.y0 }, snapTolerance, true)
	vEdges = snapByAttr(vEdges, func(e edge) float64 { return e.x0 }, snapTolerance, false)
	return append(hEdges, vEdges...)
}

func snapByAttr(edges []edge, getAttr func(edge) float64, tol float64, isHorizontal bool) []edge {
	if len(edges) == 0 {
		return edges
	}
	sort.Slice(edges, func(i, j int) bool {
		return getAttr(edges[i]) < getAttr(edges[j])
	})

	// Cluster edges within tolerance
	type cluster struct {
		edges []int
		sum   float64
	}
	var clusters []cluster
	clusters = append(clusters, cluster{edges: []int{0}, sum: getAttr(edges[0])})
	for i := 1; i < len(edges); i++ {
		last := &clusters[len(clusters)-1]
		avg := last.sum / float64(len(last.edges))
		if getAttr(edges[i])-avg <= tol {
			last.edges = append(last.edges, i)
			last.sum += getAttr(edges[i])
		} else {
			clusters = append(clusters, cluster{edges: []int{i}, sum: getAttr(edges[i])})
		}
	}

	// Move edges to cluster average
	for _, cl := range clusters {
		avg := cl.sum / float64(len(cl.edges))
		for _, idx := range cl.edges {
			if isHorizontal {
				edges[idx].y0 = avg
				edges[idx].y1 = avg
			} else {
				edges[idx].x0 = avg
				edges[idx].x1 = avg
			}
		}
	}
	return edges
}

// joinEdges merges collinear edges that are close together.
func joinEdges(edges []edge) []edge {
	type groupKey struct {
		orientation string
		pos         float64 // rounded position for grouping
	}

	groups := make(map[groupKey][]edge)
	for _, e := range edges {
		var k groupKey
		if e.orientation == "h" {
			k = groupKey{"h", math.Round(e.y0*100) / 100}
		} else {
			k = groupKey{"v", math.Round(e.x0*100) / 100}
		}
		groups[k] = append(groups[k], e)
	}

	var result []edge
	for _, group := range groups {
		if group[0].orientation == "h" {
			sort.Slice(group, func(i, j int) bool { return group[i].x0 < group[j].x0 })
		} else {
			sort.Slice(group, func(i, j int) bool { return group[i].y0 < group[j].y0 })
		}
		merged := []edge{group[0]}
		for i := 1; i < len(group); i++ {
			last := &merged[len(merged)-1]
			if group[0].orientation == "h" {
				if group[i].x0 <= last.x1+joinTolerance {
					if group[i].x1 > last.x1 {
						last.x1 = group[i].x1
					}
					continue
				}
			} else {
				if group[i].y0 <= last.y1+joinTolerance {
					if group[i].y1 > last.y1 {
						last.y1 = group[i].y1
					}
					continue
				}
			}
			merged = append(merged, group[i])
		}
		result = append(result, merged...)
	}
	return result
}

func findIntersections(edges []edge) map[[2]float64]*intersection {
	var hEdges, vEdges []edge
	for _, e := range edges {
		if e.orientation == "h" {
			hEdges = append(hEdges, e)
		} else {
			vEdges = append(vEdges, e)
		}
	}

	result := make(map[[2]float64]*intersection)
	tol := intersectionTolerance

	for vi, v := range vEdges {
		for hi, h := range hEdges {
			if v.y0 <= h.y0+tol && v.y1 >= h.y0-tol &&
				v.x0 >= h.x0-tol && v.x0 <= h.x1+tol {
				key := [2]float64{math.Round(v.x0*10) / 10, math.Round(h.y0*10) / 10}
				ix, ok := result[key]
				if !ok {
					ix = &intersection{x: v.x0, y: h.y0}
					result[key] = ix
				}
				ix.vEdges = appendUnique(ix.vEdges, vi)
				ix.hEdges = appendUnique(ix.hEdges, hi)
			}
		}
	}
	return result
}

func buildCells(intersections map[[2]float64]*intersection, edges []edge) []cell {
	// Collect sorted unique x and y positions
	xSet := make(map[float64]bool)
	ySet := make(map[float64]bool)
	for _, ix := range intersections {
		xSet[math.Round(ix.x*10)/10] = true
		ySet[math.Round(ix.y*10)/10] = true
	}
	var xs, ys []float64
	for x := range xSet {
		xs = append(xs, x)
	}
	for y := range ySet {
		ys = append(ys, y)
	}
	sort.Float64s(xs)
	sort.Float64s(ys)

	var cells []cell
	for yi := 0; yi < len(ys)-1; yi++ {
		for xi := 0; xi < len(xs)-1; xi++ {
			tl := [2]float64{xs[xi], ys[yi]}
			tr := [2]float64{xs[xi+1], ys[yi]}
			bl := [2]float64{xs[xi], ys[yi+1]}
			br := [2]float64{xs[xi+1], ys[yi+1]}
			// All 4 corners must be intersections
			if intersections[tl] != nil && intersections[tr] != nil &&
				intersections[bl] != nil && intersections[br] != nil {
				cells = append(cells, cell{
					x0: xs[xi], y0: ys[yi],
					x1: xs[xi+1], y1: ys[yi+1],
				})
			}
		}
	}
	return cells
}

func groupCellsIntoTables(cells []cell) [][]cell {
	if len(cells) == 0 {
		return nil
	}
	used := make([]bool, len(cells))
	var tables [][]cell

	for {
		start := -1
		for i, u := range used {
			if !u {
				start = i
				break
			}
		}
		if start == -1 {
			break
		}

		group := []cell{cells[start]}
		used[start] = true
		changed := true
		for changed {
			changed = false
			for i, c := range cells {
				if used[i] {
					continue
				}
				if cellTouchesGroup(c, group) {
					group = append(group, c)
					used[i] = true
					changed = true
				}
			}
		}
		if len(group) >= 2 {
			tables = append(tables, group)
		}
	}
	return tables
}

func cellTouchesGroup(c cell, group []cell) bool {
	for _, g := range group {
		if sharesCorner(c, g) {
			return true
		}
	}
	return false
}

func sharesCorner(a, b cell) bool {
	corners := [][2]float64{{a.x0, a.y0}, {a.x1, a.y0}, {a.x0, a.y1}, {a.x1, a.y1}}
	bCorners := [][2]float64{{b.x0, b.y0}, {b.x1, b.y0}, {b.x0, b.y1}, {b.x1, b.y1}}
	for _, ac := range corners {
		for _, bc := range bCorners {
			if math.Abs(ac[0]-bc[0]) < 1 && math.Abs(ac[1]-bc[1]) < 1 {
				return true
			}
		}
	}
	return false
}

func renderTableMarkdown(cells []cell, page *mupdf.Page) string {
	if len(cells) == 0 {
		return ""
	}

	// Find unique row/col boundaries
	xSet := make(map[float64]bool)
	ySet := make(map[float64]bool)
	for _, c := range cells {
		xSet[c.x0] = true
		xSet[c.x1] = true
		ySet[c.y0] = true
		ySet[c.y1] = true
	}
	var xs, ys []float64
	for x := range xSet {
		xs = append(xs, x)
	}
	for y := range ySet {
		ys = append(ys, y)
	}
	sort.Float64s(xs)
	sort.Float64s(ys)

	nRows := len(ys) - 1
	nCols := len(xs) - 1
	if nRows < 1 || nCols < 1 {
		return ""
	}

	// Map characters to cells
	grid := make([][]string, nRows)
	for r := range grid {
		grid[r] = make([]string, nCols)
	}

	for _, block := range page.Blocks {
		if block.Type != 0 {
			continue
		}
		for _, line := range block.Lines {
			prevCol, prevRow := -1, -1
			for _, ch := range line.Chars {
				cx, cy := ch.Origin[0], ch.Origin[1]
				col := -1
				for c := 0; c < nCols; c++ {
					if cx >= xs[c]-1 && cx < xs[c+1]+1 {
						col = c
						break
					}
				}
				row := -1
				for r := 0; r < nRows; r++ {
					if cy >= ys[r]-1 && cy < ys[r+1]+1 {
						row = r
						break
					}
				}
				if row >= 0 && col >= 0 {
					// Add space when entering a new word (gap between chars)
					if row == prevRow && col == prevCol && ch.Rune > ' ' {
						cellText := grid[row][col]
						if len(cellText) > 0 {
							lastRune := rune(cellText[len(cellText)-1])
							if lastRune != ' ' && lastRune != '-' {
								// Check for x-gap indicating word boundary
								// Simple heuristic: if we skipped a space char, add one
							}
						}
					}
					if ch.Rune == ' ' {
						if len(grid[row][col]) > 0 && grid[row][col][len(grid[row][col])-1] != ' ' {
							grid[row][col] += " "
						}
					} else {
						grid[row][col] += string(ch.Rune)
					}
					prevCol = col
					prevRow = row
				}
			}
		}
	}

	// Clean cells
	for r := range grid {
		for c := range grid[r] {
			grid[r][c] = strings.TrimSpace(grid[r][c])
		}
	}

	// Require at least 30% of cells to have content (filter diagram false positives)
	totalCells := nRows * nCols
	filledCells := 0
	for _, row := range grid {
		for _, c := range row {
			if c != "" {
				filledCells++
			}
		}
	}
	if filledCells == 0 || float64(filledCells)/float64(totalCells) < 0.3 {
		return ""
	}

	// Render
	var out strings.Builder
	for r, row := range grid {
		out.WriteString("|")
		for _, cell := range row {
			text := strings.ReplaceAll(cell, "|", "\\|")
			text = strings.ReplaceAll(text, "\n", " ")
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
	return out.String()
}

func tableBBox(cells []cell) mupdf.Rect {
	r := mupdf.Rect{X0: cells[0].x0, Y0: cells[0].y0, X1: cells[0].x1, Y1: cells[0].y1}
	for _, c := range cells[1:] {
		r.X0 = math.Min(r.X0, c.x0)
		r.Y0 = math.Min(r.Y0, c.y0)
		r.X1 = math.Max(r.X1, c.x1)
		r.Y1 = math.Max(r.Y1, c.y1)
	}
	return r
}

func edgeLength(e edge) float64 {
	dx := e.x1 - e.x0
	dy := e.y1 - e.y0
	return math.Sqrt(dx*dx + dy*dy)
}

func appendUnique(s []int, v int) []int {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}
