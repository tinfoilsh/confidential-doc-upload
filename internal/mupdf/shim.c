#include "shim.h"

fz_context* mupdf_new_context(void) {
    fz_context *ctx = fz_new_context(NULL, NULL, FZ_STORE_DEFAULT);
    if (ctx)
        fz_register_document_handlers(ctx);
    return ctx;
}

fz_document* mupdf_open_document(fz_context *ctx, const void *data, size_t len, int *errcode) {
    fz_document *doc = NULL;
    fz_buffer *buf = NULL;
    *errcode = 0;
    fz_try(ctx) {
        buf = fz_new_buffer_from_shared_data(ctx, data, len);
        doc = fz_open_document_with_buffer(ctx, "application/pdf", buf);
    }
    fz_catch(ctx) {
        *errcode = 1;
        if (buf) fz_drop_buffer(ctx, buf);
        return NULL;
    }
    /* Buffer ownership stays with Go (shared data, not copied).
       The document holds a reference to the buffer internally.
       Go must keep the []byte alive for the document's lifetime. */
    return doc;
}

int mupdf_count_pages(fz_context *ctx, fz_document *doc) {
    int count = 0;
    fz_try(ctx) {
        count = fz_count_pages(ctx, doc);
    }
    fz_catch(ctx) {
        return -1;
    }
    return count;
}

fz_page* mupdf_load_page(fz_context *ctx, fz_document *doc, int number, int *errcode) {
    fz_page *page = NULL;
    *errcode = 0;
    fz_try(ctx) {
        page = fz_load_page(ctx, doc, number);
    }
    fz_catch(ctx) {
        *errcode = 1;
        return NULL;
    }
    return page;
}

fz_stext_page* mupdf_extract_stext(fz_context *ctx, fz_page *page, int flags, int *errcode) {
    fz_stext_page *tp = NULL;
    fz_stext_options opts = { flags };
    fz_device *dev = NULL;
    *errcode = 0;
    fz_try(ctx) {
        fz_rect mediabox = fz_bound_page(ctx, page);
        tp = fz_new_stext_page(ctx, mediabox);
        dev = fz_new_stext_device(ctx, tp, &opts);
        fz_run_page(ctx, page, dev, fz_identity, NULL);
        fz_close_device(ctx, dev);
    }
    fz_catch(ctx) {
        *errcode = 1;
        if (dev) fz_drop_device(ctx, dev);
        if (tp) fz_drop_stext_page(ctx, tp);
        return NULL;
    }
    fz_drop_device(ctx, dev);
    return tp;
}

/* Line segment extraction via path walker + device */
typedef struct {
    mupdf_line_segment *segments;
    int count;
    int max;
    float snap_tol;
    float cx, cy, mx, my;
    fz_matrix ctm;
} walk_state;

static void walk_moveto(fz_context *ctx, void *arg, float x, float y) {
    walk_state *s = (walk_state*)arg;
    s->mx = s->cx = x; s->my = s->cy = y;
}

static void walk_lineto(fz_context *ctx, void *arg, float x, float y) {
    walk_state *s = (walk_state*)arg;
    if (s->count < s->max) {
        fz_point p0 = fz_transform_point_xy(s->cx, s->cy, s->ctm);
        fz_point p1 = fz_transform_point_xy(x, y, s->ctm);
        mupdf_line_segment *seg = &s->segments[s->count];
        seg->x0 = p0.x; seg->y0 = p0.y;
        seg->x1 = p1.x; seg->y1 = p1.y;
        float dx = seg->x1 - seg->x0;
        float dy = seg->y1 - seg->y0;
        if (dx < 0) dx = -dx;
        if (dy < 0) dy = -dy;
        if (dy <= s->snap_tol) seg->is_horizontal = 1;
        else if (dx <= s->snap_tol) seg->is_horizontal = 0;
        else seg->is_horizontal = -1;
        s->count++;
    }
    s->cx = x; s->cy = y;
}

static void walk_curveto(fz_context *ctx, void *arg, float x1, float y1, float x2, float y2, float x3, float y3) {
    walk_state *s = (walk_state*)arg;
    s->cx = x3; s->cy = y3;
}

static void walk_closepath(fz_context *ctx, void *arg) {
    walk_state *s = (walk_state*)arg;
    /* Close path = line back to move-to point */
    walk_lineto(NULL, arg, s->mx, s->my);
}

static void walk_rectto(fz_context *ctx, void *arg, float x1, float y1, float x2, float y2) {
    walk_moveto(ctx, arg, x1, y1);
    walk_lineto(ctx, arg, x2, y1);
    walk_lineto(ctx, arg, x2, y2);
    walk_lineto(ctx, arg, x1, y2);
    walk_lineto(ctx, arg, x1, y1);
}

typedef struct {
    fz_device super;
    walk_state ws;
} line_trace_device;

static void dev_stroke_path(fz_context *ctx, fz_device *dev_,
    const fz_path *path, const fz_stroke_state *stroke,
    fz_matrix ctm, fz_colorspace *cs, const float *color, float alpha,
    fz_color_params cp)
{
    line_trace_device *dev = (line_trace_device*)dev_;
    static const fz_path_walker walker = {
        walk_moveto, walk_lineto, walk_curveto, walk_closepath,
        NULL, NULL, NULL, walk_rectto
    };
    dev->ws.ctm = ctm;
    fz_walk_path(ctx, path, &walker, &dev->ws);
}

int mupdf_extract_line_segments(fz_context *ctx, fz_page *page,
    mupdf_line_segment *out, int max_segments, int *errcode)
{
    *errcode = 0;
    line_trace_device dev;
    memset(&dev, 0, sizeof(dev));
    dev.ws.segments = out;
    dev.ws.max = max_segments;
    dev.ws.count = 0;
    dev.ws.snap_tol = 3.0f;
    dev.super.stroke_path = dev_stroke_path;

    fz_try(ctx) {
        fz_run_page_contents(ctx, page, &dev.super, fz_identity, NULL);
    }
    fz_catch(ctx) {
        *errcode = 1;
        return 0;
    }
    return dev.ws.count;
}

fz_buffer* mupdf_render_page_png(fz_context *ctx, fz_page *page, float zoom, int *errcode) {
    fz_pixmap *pix = NULL;
    fz_buffer *buf = NULL;
    *errcode = 0;
    fz_try(ctx) {
        fz_matrix ctm = fz_scale(zoom, zoom);
        pix = fz_new_pixmap_from_page(ctx, page, ctm, fz_device_rgb(ctx), 0);
        buf = fz_new_buffer_from_pixmap_as_png(ctx, pix, fz_default_color_params);
    }
    fz_catch(ctx) {
        *errcode = 1;
        if (pix) fz_drop_pixmap(ctx, pix);
        return NULL;
    }
    fz_drop_pixmap(ctx, pix);
    return buf;
}
