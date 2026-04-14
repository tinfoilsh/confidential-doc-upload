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
