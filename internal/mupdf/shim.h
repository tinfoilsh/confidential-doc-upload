#ifndef MUPDF_SHIM_H
#define MUPDF_SHIM_H

#include <mupdf/fitz.h>

fz_context* mupdf_new_context(void);
fz_document* mupdf_open_document(fz_context *ctx, const void *data, size_t len, int *errcode);
int mupdf_count_pages(fz_context *ctx, fz_document *doc);
fz_page* mupdf_load_page(fz_context *ctx, fz_document *doc, int number, int *errcode);
fz_stext_page* mupdf_extract_stext(fz_context *ctx, fz_page *page, int flags, int *errcode);
fz_buffer* mupdf_render_page_png(fz_context *ctx, fz_page *page, float zoom, int *errcode);

static inline fz_stext_line* mupdf_block_first_line(fz_stext_block *b) { return b->u.t.first_line; }

#endif
