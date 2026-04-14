package mupdf

/*
#cgo CFLAGS: -I/usr/local/include
#cgo LDFLAGS: -L/usr/local/lib -lmupdf -lmupdf-third -lm
#include "shim.h"
#include <stdlib.h>
*/
import "C"

import (
	"fmt"
	"unsafe"
)

type Rect struct {
	X0, Y0, X1, Y1 float64
}

func (r Rect) Width() float64  { return r.X1 - r.X0 }
func (r Rect) Height() float64 { return r.Y1 - r.Y0 }

type TextChar struct {
	Rune      rune
	Origin    [2]float64
	Size      float64
	FontName  string
	Bold      bool
	Italic    bool
	Mono      bool
	Strikeout bool
	Color     int
	Flags     int // raw MuPDF char flags
}

type TextLine struct {
	BBox  Rect
	Dir   [2]float64
	WMode int
	Chars []TextChar
}

type GridCell struct {
	Row, Col int
	Text     string
}

type TextBlock struct {
	Type    int // 0=text, 1=image, 4=grid(table)
	BBox    Rect
	Lines   []TextLine
	GridXs  []float64 // column boundary positions (for grid blocks)
	GridYs  []float64 // row boundary positions (for grid blocks)
}

type Page struct {
	Blocks       []TextBlock
	LineSegments []LineSegment
	MediaBox     Rect
	PageNum      int
	CharCount    int
}

type Document struct {
	ctx   *C.fz_context
	doc   *C.fz_document
	cdata unsafe.Pointer // C-allocated copy of PDF bytes, freed on Close
}

func OpenFromBytes(data []byte) (*Document, error) {
	ctx := C.mupdf_new_context()
	if ctx == nil {
		return nil, fmt.Errorf("mupdf: failed to create context")
	}

	// Copy data to C memory so MuPDF can safely access it during
	// lazy page loading and PDF repair (Go GC must not move it).
	cdata := C.CBytes(data)

	var errcode C.int
	doc := C.mupdf_open_document(ctx, cdata, C.size_t(len(data)), &errcode)
	if errcode != 0 || doc == nil {
		C.free(cdata)
		C.fz_drop_context(ctx)
		return nil, fmt.Errorf("mupdf: failed to open document")
	}

	return &Document{ctx: ctx, doc: doc, cdata: cdata}, nil
}

func (d *Document) Close() {
	if d.doc != nil {
		C.fz_drop_document(d.ctx, d.doc)
		d.doc = nil
	}
	if d.cdata != nil {
		C.free(d.cdata)
		d.cdata = nil
	}
	if d.ctx != nil {
		C.fz_drop_context(d.ctx)
		d.ctx = nil
	}
}

func (d *Document) PageCount() int {
	return int(C.mupdf_count_pages(d.ctx, d.doc))
}

// ExtractPage extracts text and line segments from a page in a single pass.
func (d *Document) ExtractPage(pageNum int) (*Page, error) {
	return d.extractPage(pageNum, true)
}

func (d *Document) extractPage(pageNum int, withLineSegments bool) (*Page, error) {
	var errcode C.int
	cpage := C.mupdf_load_page(d.ctx, d.doc, C.int(pageNum), &errcode)
	if errcode != 0 || cpage == nil {
		return nil, fmt.Errorf("mupdf: failed to load page %d", pageNum)
	}
	defer C.fz_drop_page(d.ctx, cpage)

	flags := C.FZ_STEXT_PRESERVE_WHITESPACE | C.FZ_STEXT_PRESERVE_LIGATURES | C.FZ_STEXT_COLLECT_STYLES

	const maxSegments = 10000
	var segments []C.mupdf_line_segment
	var segCount C.int
	var stext *C.fz_stext_page

	if withLineSegments {
		segments = make([]C.mupdf_line_segment, maxSegments)
		stext = C.mupdf_extract_all(d.ctx, cpage, C.int(flags),
			&segments[0], C.int(maxSegments), &segCount, &errcode)
	} else {
		stext = C.mupdf_extract_stext(d.ctx, cpage, C.int(flags), &errcode)
	}
	if errcode != 0 || stext == nil {
		return nil, fmt.Errorf("mupdf: failed to extract text from page %d", pageNum)
	}
	defer C.fz_drop_stext_page(d.ctx, stext)

	mediabox := stext.mediabox
	page := &Page{
		MediaBox: rectFromC(mediabox),
		PageNum:  pageNum,
	}

	for block := stext.first_block; block != nil; block = block.next {
		tb := TextBlock{
			Type: int(block._type),
			BBox: rectFromC(block.bbox),
		}

		// Extract grid positions for table blocks
		if block._type == 4 { // FZ_STEXT_BLOCK_GRID
			xs := C.mupdf_block_grid_xs(block)
			ys := C.mupdf_block_grid_ys(block)
			nxs := int(C.mupdf_grid_len(xs))
			nys := int(C.mupdf_grid_len(ys))
			for i := 0; i < nxs; i++ {
				tb.GridXs = append(tb.GridXs, float64(C.mupdf_grid_pos(xs, C.int(i))))
			}
			for i := 0; i < nys; i++ {
				tb.GridYs = append(tb.GridYs, float64(C.mupdf_grid_pos(ys, C.int(i))))
			}
		}

		if block._type == C.FZ_STEXT_BLOCK_TEXT {
			for line := C.mupdf_block_first_line(block); line != nil; line = line.next {
				tl := TextLine{
					BBox:  rectFromC(line.bbox),
					Dir:   [2]float64{float64(line.dir.x), float64(line.dir.y)},
					WMode: int(line.wmode),
				}

				for ch := line.first_char; ch != nil; ch = ch.next {
					font := ch.font
					var fontName string
					var bold, italic, mono bool
					if font != nil {
						cname := C.fz_font_name(d.ctx, font)
						if cname != nil {
							fontName = C.GoString(cname)
						}
						bold = C.fz_font_is_bold(d.ctx, font) != 0
						italic = C.fz_font_is_italic(d.ctx, font) != 0
						mono = C.fz_font_is_monospaced(d.ctx, font) != 0
					}

					charFlags := int(ch.flags)
					// Use per-char BOLD flag if available (from FZ_STEXT_COLLECT_STYLES)
					charBold := bold || (charFlags&8 != 0) // FZ_STEXT_BOLD = 8
					charStrikeout := charFlags&1 != 0      // FZ_STEXT_STRIKEOUT = 1

					tc := TextChar{
						Rune:      rune(ch.c),
						Origin:    [2]float64{float64(ch.origin.x), float64(ch.origin.y)},
						Size:      float64(ch.size),
						FontName:  fontName,
						Bold:      charBold,
						Italic:    italic,
						Mono:      mono,
						Strikeout: charStrikeout,
						Color:     int(ch.argb),
						Flags:     charFlags,
					}
					tl.Chars = append(tl.Chars, tc)
					page.CharCount++
				}
				tb.Lines = append(tb.Lines, tl)
			}
		}
		page.Blocks = append(page.Blocks, tb)
	}

	// Populate line segments from the combined extraction
	if withLineSegments && segCount > 0 {
		for i := 0; i < int(segCount); i++ {
			s := segments[i]
			page.LineSegments = append(page.LineSegments, LineSegment{
				X0:           float64(s.x0),
				Y0:           float64(s.y0),
				X1:           float64(s.x1),
				Y1:           float64(s.y1),
				IsHorizontal: int(s.is_horizontal),
			})
		}
	}

	return page, nil
}

type LineSegment struct {
	X0, Y0, X1, Y1 float64
	IsHorizontal    int // 1=horizontal, 0=vertical, -1=diagonal
}

func (d *Document) ExtractLineSegments(pageNum int) ([]LineSegment, error) {
	var errcode C.int
	cpage := C.mupdf_load_page(d.ctx, d.doc, C.int(pageNum), &errcode)
	if errcode != 0 || cpage == nil {
		return nil, fmt.Errorf("mupdf: failed to load page %d", pageNum)
	}
	defer C.fz_drop_page(d.ctx, cpage)

	const maxSegments = 10000
	segments := make([]C.mupdf_line_segment, maxSegments)
	count := C.mupdf_extract_line_segments(d.ctx, cpage, &segments[0], C.int(maxSegments), &errcode)
	if errcode != 0 {
		return nil, fmt.Errorf("mupdf: failed to extract line segments from page %d", pageNum)
	}

	result := make([]LineSegment, int(count))
	for i := 0; i < int(count); i++ {
		s := segments[i]
		result[i] = LineSegment{
			X0:           float64(s.x0),
			Y0:           float64(s.y0),
			X1:           float64(s.x1),
			Y1:           float64(s.y1),
			IsHorizontal: int(s.is_horizontal),
		}
	}
	return result, nil
}

func (d *Document) RenderPagePNG(pageNum int, dpi int) ([]byte, error) {
	var errcode C.int
	cpage := C.mupdf_load_page(d.ctx, d.doc, C.int(pageNum), &errcode)
	if errcode != 0 || cpage == nil {
		return nil, fmt.Errorf("mupdf: failed to load page %d", pageNum)
	}
	defer C.fz_drop_page(d.ctx, cpage)

	zoom := float64(dpi) / 72.0
	buf := C.mupdf_render_page_png(d.ctx, cpage, C.float(zoom), &errcode)
	if errcode != 0 || buf == nil {
		return nil, fmt.Errorf("mupdf: failed to render page %d", pageNum)
	}
	defer C.fz_drop_buffer(d.ctx, buf)

	dataLen := C.fz_buffer_storage(d.ctx, buf, nil)
	dataPtr := C.fz_string_from_buffer(d.ctx, buf)
	return C.GoBytes(unsafe.Pointer(dataPtr), C.int(dataLen)), nil
}

func rectFromC(r C.fz_rect) Rect {
	return Rect{
		X0: float64(r.x0), Y0: float64(r.y0),
		X1: float64(r.x1), Y1: float64(r.y1),
	}
}
