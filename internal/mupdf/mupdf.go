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

type TextBlock struct {
	Type  int // 0=text, 1=image
	BBox  Rect
	Lines []TextLine
}

type Page struct {
	Blocks    []TextBlock
	MediaBox  Rect
	PageNum   int
	CharCount int
}

type Document struct {
	ctx *C.fz_context
	doc *C.fz_document
}

func OpenFromBytes(data []byte) (*Document, error) {
	ctx := C.mupdf_new_context()
	if ctx == nil {
		return nil, fmt.Errorf("mupdf: failed to create context")
	}

	var errcode C.int
	doc := C.mupdf_open_document(ctx, unsafe.Pointer(&data[0]), C.size_t(len(data)), &errcode)
	if errcode != 0 || doc == nil {
		C.fz_drop_context(ctx)
		return nil, fmt.Errorf("mupdf: failed to open document")
	}

	return &Document{ctx: ctx, doc: doc}, nil
}

func (d *Document) Close() {
	if d.doc != nil {
		C.fz_drop_document(d.ctx, d.doc)
		d.doc = nil
	}
	if d.ctx != nil {
		C.fz_drop_context(d.ctx)
		d.ctx = nil
	}
}

func (d *Document) PageCount() int {
	return int(C.mupdf_count_pages(d.ctx, d.doc))
}

func (d *Document) ExtractPage(pageNum int) (*Page, error) {
	var errcode C.int
	cpage := C.mupdf_load_page(d.ctx, d.doc, C.int(pageNum), &errcode)
	if errcode != 0 || cpage == nil {
		return nil, fmt.Errorf("mupdf: failed to load page %d", pageNum)
	}
	defer C.fz_drop_page(d.ctx, cpage)

	flags := C.FZ_STEXT_PRESERVE_WHITESPACE | C.FZ_STEXT_PRESERVE_LIGATURES | C.FZ_STEXT_COLLECT_STYLES
	stext := C.mupdf_extract_stext(d.ctx, cpage, C.int(flags), &errcode)
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

	return page, nil
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
