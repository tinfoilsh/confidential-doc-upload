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

/* Grid (table) accessors */
static inline fz_stext_grid_positions* mupdf_block_grid_xs(fz_stext_block *b) { return b->u.b.xs; }
static inline fz_stext_grid_positions* mupdf_block_grid_ys(fz_stext_block *b) { return b->u.b.ys; }
static inline int mupdf_grid_len(fz_stext_grid_positions *g) { return g ? g->len : 0; }
static inline float mupdf_grid_pos(fz_stext_grid_positions *g, int i) { return g->list[i].pos; }

/* Drawing/path extraction for table detection */
typedef struct {
    float x0, y0, x1, y1;
    int is_horizontal; /* 1 if horizontal, 0 if vertical, -1 if neither */
} mupdf_line_segment;

int mupdf_extract_line_segments(fz_context *ctx, fz_page *page,
    mupdf_line_segment *out, int max_segments, int *errcode);

fz_stext_page* mupdf_extract_all(fz_context *ctx, fz_page *page, int stext_flags,
    mupdf_line_segment *seg_out, int seg_max, int *seg_count, int *errcode);

#endif
