package server

import (
	"context"
	"fmt"
	"log/slog"
	"math/bits"
	"strings"
	"time"
)

// nextPow2 rounds n up to the nearest power of 2.
// Used for size metrics to prevent document fingerprinting via exact byte counts.
func nextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

type PageResult struct {
	Page      int    `json:"page"`
	Image     string `json:"image,omitempty"`
	IsScanned bool   `json:"is_scanned"`
}

type ConvertResult struct {
	MDContent string       `json:"md_content"`
	Pages     []PageResult `json:"pages,omitempty"`
}

// mode "text"   (default): text extraction + VLM OCR for scanned pages only. No VLM for born-digital.
// mode "vision":           text + VLM OCR for scanned + VLM visual descriptions for born-digital.
// mode "images":           text where available + page images, no VLM — for vision-capable downstream models.
// mode "raw":              text layer only, no VLM, no images — fastest possible.
// mode "vlm":              VLM full-page OCR on every page — highest quality, slowest.
func convertFile(ctx context.Context, data []byte, filename string, mode string) (ConvertResult, error) {
	t0 := time.Now()

	extracted, err := sidecarExtract(ctx, data, filename)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("extract: %w", err)
	}

	// Non-PDF/image formats: direct markdown, no VLM needed
	if extracted.Format != "pdf" && extracted.Format != "image" {
		slog.Info("processed", "file", filename, "format", extracted.Format,
			"elapsed", time.Since(t0).Seconds())
		return ConvertResult{MDContent: extracted.MDContent}, nil
	}

	// Standalone image file
	if extracted.Format == "image" {
		if mode == "raw" {
			return ConvertResult{}, nil
		}
		return convertImage(ctx, data, filename, mode)
	}

	// PDF
	return convertPDF(ctx, data, filename, mode, extracted)
}

func convertImage(ctx context.Context, data []byte, filename string, mode string) (ConvertResult, error) {
	rendered, err := sidecarRender(ctx, data, filename, 100)
	if err != nil || len(rendered.Pages) == 0 {
		return ConvertResult{}, err
	}
	img := rendered.Pages[0].Image

	result := ConvertResult{}
	if mode == "images" {
		result.Pages = []PageResult{{Page: 1, Image: img, IsScanned: true}}
	} else {
		md, err := vlmFullPageOCR(ctx, img)
		if err != nil {
			return ConvertResult{}, fmt.Errorf("vlm image: %w", err)
		}
		result.MDContent = md
	}
	return result, nil
}

func convertPDF(ctx context.Context, data []byte, filename, mode string, extracted ExtractResult) (ConvertResult, error) {
	t0 := time.Now()
	nPages := len(extracted.Pages)

	var scannedIdxs []int
	textPages := make(map[int]string)
	scannedSet := make(map[int]bool)
	for _, p := range extracted.Pages {
		if p.IsScanned {
			scannedIdxs = append(scannedIdxs, p.Page)
			scannedSet[p.Page] = true
		} else {
			textPages[p.Page] = p.Text
		}
	}

	slog.Info("classified", "file", filename, "pages", nPages,
		"scanned", len(scannedIdxs), "born_digital", len(textPages), "mode", mode)

	// Record document metrics (coarse-grained for privacy).
	// Size is rounded to next power of 2 to prevent document fingerprinting.
	metricPages.Observe(float64(nPages))
	metricSize.Observe(float64(nextPow2(len(data))))
	if len(scannedIdxs) == 0 {
		metricDocType.WithLabelValues("born_digital").Inc()
	} else if len(textPages) == 0 {
		metricDocType.WithLabelValues("scanned").Inc()
	} else {
		metricDocType.WithLabelValues("mixed").Inc()
	}

	switch mode {
	case "raw":
		return convertPDFRaw(nPages, textPages)
	case "images":
		return convertPDFImages(ctx, data, filename, nPages, textPages, scannedSet)
	case "vlm":
		return convertPDFVLM(ctx, data, filename, nPages, t0)
	case "vision":
		return convertPDFVision(ctx, data, filename, nPages, scannedIdxs, textPages, scannedSet, t0)
	default: // "text"
		return convertPDFText(ctx, data, filename, nPages, scannedIdxs, textPages, t0)
	}
}

// mode=images: return text where available + page images, no VLM
func convertPDFImages(ctx context.Context, data []byte, filename string, nPages int, textPages map[int]string, scannedSet map[int]bool) (ConvertResult, error) {
	rendered, err := sidecarRender(ctx, data, filename, 100)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("render: %w", err)
	}

	renderedMap := make(map[int]string)
	for _, rp := range rendered.Pages {
		renderedMap[rp.Page] = rp.Image
	}

	var parts []string
	var pages []PageResult
	for i := 1; i <= nPages; i++ {
		parts = append(parts, textPages[i])
		pages = append(pages, PageResult{
			Page:      i,
			Image:     renderedMap[i],
			IsScanned: scannedSet[i],
		})
	}

	return ConvertResult{
		MDContent: strings.Join(parts, "\n\n---\n\n"),
		Pages:     pages,
	}, nil
}

// mode=raw: text layer only, no VLM, no rendering — fastest possible
func convertPDFRaw(nPages int, textPages map[int]string) (ConvertResult, error) {
	var parts []string
	for i := 1; i <= nPages; i++ {
		parts = append(parts, textPages[i])
	}
	return ConvertResult{
		MDContent: strings.Join(parts, "\n\n---\n\n"),
	}, nil
}

// mode=vlm: Gemma full-page OCR on every page — highest quality
func convertPDFVLM(ctx context.Context, data []byte, filename string, nPages int, t0 time.Time) (ConvertResult, error) {
	rendered, err := sidecarRender(ctx, data, filename, 150)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("render: %w", err)
	}

	allWork := make(map[int]vlmWorkItem)
	for _, rp := range rendered.Pages {
		allWork[rp.Page] = vlmWorkItem{image: rp.Image, fn: vlmFullPageOCR, kind: "ocr"}
	}

	vlmResults := vlmParallelMixed(ctx, allWork)

	parts := make([]string, nPages)
	var failed []int
	var firstErr error
	for i := 1; i <= nPages; i++ {
		res, ok := vlmResults[i]
		if !ok || res.err != nil {
			failed = append(failed, i)
			if firstErr == nil {
				if ok {
					firstErr = res.err
				} else {
					firstErr = fmt.Errorf("page %d: missing result", i)
				}
			}
			continue
		}
		parts[i-1] = res.text
	}
	if len(failed) > 0 {
		return ConvertResult{}, fmt.Errorf("vlm OCR failed for %d/%d pages %v: %w", len(failed), nPages, failed, firstErr)
	}

	slog.Info("processed", "file", filename, "pages", nPages,
		"mode", "vlm", "elapsed", time.Since(t0).Seconds())

	return ConvertResult{
		MDContent: strings.Join(parts, "\n\n---\n\n"),
	}, nil
}

// mode=text (default): text extraction for born-digital, VLM OCR for scanned only.
// No VLM calls for born-digital pages — fast for native PDFs.
func convertPDFText(ctx context.Context, data []byte, filename string, nPages int, scannedIdxs []int, textPages map[int]string, t0 time.Time) (ConvertResult, error) {
	// Only send scanned pages to VLM
	if len(scannedIdxs) > 0 {
		rendered, err := sidecarRender(ctx, data, filename, 100)
		if err != nil {
			return ConvertResult{}, fmt.Errorf("render: %w", err)
		}
		renderedImages := make(map[int]string)
		for _, rp := range rendered.Pages {
			renderedImages[rp.Page] = rp.Image
		}

		vlmWork := make(map[int]vlmWorkItem)
		for _, idx := range scannedIdxs {
			if img, ok := renderedImages[idx]; ok {
				vlmWork[idx] = vlmWorkItem{image: img, fn: vlmFullPageOCR, kind: "ocr"}
			}
		}

		if len(vlmWork) > 0 {
			vlmResults := vlmParallelMixed(ctx, vlmWork)
			var failed []int
			var firstErr error
			for idx, res := range vlmResults {
				if res.err != nil {
					failed = append(failed, idx)
					if firstErr == nil {
						firstErr = res.err
					}
					continue
				}
				textPages[idx] = res.text
			}
			if len(failed) > 0 {
				return ConvertResult{}, fmt.Errorf("vlm OCR failed for %d scanned page(s) %v: %w", len(failed), failed, firstErr)
			}
		}
	}

	var parts []string
	for i := 1; i <= nPages; i++ {
		parts = append(parts, textPages[i])
	}

	slog.Info("processed", "file", filename, "pages", nPages,
		"scanned", len(scannedIdxs), "mode", "text",
		"elapsed", time.Since(t0).Seconds())

	return ConvertResult{
		MDContent: strings.Join(parts, "\n\n---\n\n"),
	}, nil
}

// mode=vision: text + VLM OCR for scanned + VLM visual descriptions for born-digital.
// Sends every page to VLM — slow but comprehensive.
func convertPDFVision(ctx context.Context, data []byte, filename string, nPages int, scannedIdxs []int, textPages map[int]string, scannedSet map[int]bool, t0 time.Time) (ConvertResult, error) {
	rendered, err := sidecarRender(ctx, data, filename, 100)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("render: %w", err)
	}
	renderedImages := make(map[int]string)
	for _, rp := range rendered.Pages {
		renderedImages[rp.Page] = rp.Image
	}

	allVLMWork := make(map[int]vlmWorkItem)
	for _, idx := range scannedIdxs {
		if img, ok := renderedImages[idx]; ok {
			allVLMWork[idx] = vlmWorkItem{image: img, fn: vlmFullPageOCR, kind: "ocr"}
		}
	}
	for page := range textPages {
		if img, ok := renderedImages[page]; ok {
			allVLMWork[page] = vlmWorkItem{image: img, fn: vlmVisualExtract, kind: "visual"}
		}
	}

	vlmResults := vlmParallelMixed(ctx, allVLMWork)

	visualDescs := make(map[int]string)
	var ocrFailed []int
	var firstOCRErr error
	for idx, res := range vlmResults {
		work := allVLMWork[idx]
		if res.err != nil {
			slog.Warn("vlm failed", "page", idx, "kind", work.kind, "err", res.err)
			if work.kind == "ocr" {
				ocrFailed = append(ocrFailed, idx)
				if firstOCRErr == nil {
					firstOCRErr = res.err
				}
			}
			continue
		}
		if work.kind == "ocr" {
			textPages[idx] = res.text
		} else {
			d := strings.TrimSpace(res.text)
			if !strings.HasPrefix(strings.ToUpper(d), "NONE") && len(d) > 10 {
				visualDescs[idx] = d
			}
		}
	}
	if len(ocrFailed) > 0 {
		return ConvertResult{}, fmt.Errorf("vlm OCR failed for %d scanned page(s) %v: %w", len(ocrFailed), ocrFailed, firstOCRErr)
	}

	var parts []string
	for i := 1; i <= nPages; i++ {
		md := textPages[i]
		if desc, ok := visualDescs[i]; ok {
			md += "\n\n## Visual Elements\n\n" + desc
		}
		parts = append(parts, md)
	}

	slog.Info("processed", "file", filename, "pages", nPages,
		"scanned", len(scannedIdxs), "visual_descs", len(visualDescs),
		"mode", "vision", "elapsed", time.Since(t0).Seconds())

	return ConvertResult{
		MDContent: strings.Join(parts, "\n\n---\n\n"),
	}, nil
}
