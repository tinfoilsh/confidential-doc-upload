package server

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type PageResult struct {
	Page      int    `json:"page"`
	Image     string `json:"image,omitempty"`
	IsScanned bool   `json:"is_scanned"`
}

type ConvertResult struct {
	MDContent string       `json:"md_content"`
	Pages     []PageResult `json:"pages,omitempty"`
}

// mode "text"   (default): full markdown, Gemma OCR for scanned, Gemma visual descriptions for born-digital
// mode "images": text where available + page images, no VLM — fast, for vision-capable downstream models
// mode "raw":    text layer only, no VLM, no images — fastest possible
// mode "vlm":    Gemma full-page OCR on every page — highest quality, slowest
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

	switch mode {
	case "raw":
		return convertPDFRaw(nPages, textPages)
	case "images":
		return convertPDFImages(ctx, data, filename, nPages, textPages, scannedSet)
	case "vlm":
		return convertPDFVLM(ctx, data, filename, nPages, t0)
	default:
		return convertPDFText(ctx, data, filename, nPages, scannedIdxs, textPages, scannedSet, t0)
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

	var parts []string
	for i := 1; i <= nPages; i++ {
		if res, ok := vlmResults[i]; ok && res.err == nil {
			parts = append(parts, res.text)
		} else {
			slog.Warn("vlm ocr failed", "page", i)
			parts = append(parts, "[OCR failed]")
		}
	}

	slog.Info("processed", "file", filename, "pages", nPages,
		"mode", "vlm", "elapsed", time.Since(t0).Seconds())

	return ConvertResult{
		MDContent: strings.Join(parts, "\n\n---\n\n"),
	}, nil
}

// mode=text (default): full extraction with VLM OCR + visual descriptions
func convertPDFText(ctx context.Context, data []byte, filename string, nPages int, scannedIdxs []int, textPages map[int]string, scannedSet map[int]bool, t0 time.Time) (ConvertResult, error) {
	needsRender := len(scannedIdxs) > 0 || len(textPages) > 0
	var renderedImages map[int]string
	if needsRender {
		rendered, err := sidecarRender(ctx, data, filename, 100)
		if err != nil {
			return ConvertResult{}, fmt.Errorf("render: %w", err)
		}
		renderedImages = make(map[int]string)
		for _, rp := range rendered.Pages {
			renderedImages[rp.Page] = rp.Image
		}
	}

	allVLMWork := make(map[int]vlmWorkItem)

	for _, idx := range scannedIdxs {
		if img, ok := renderedImages[idx]; ok {
			allVLMWork[idx] = vlmWorkItem{image: img, fn: vlmFullPageOCR, kind: "ocr"}
		}
	}

	for page, _ := range textPages {
		if img, ok := renderedImages[page]; ok {
			allVLMWork[page] = vlmWorkItem{image: img, fn: vlmVisualExtract, kind: "visual"}
		}
	}

	vlmResults := vlmParallelMixed(ctx, allVLMWork)

	visualDescs := make(map[int]string)
	for idx, res := range vlmResults {
		work := allVLMWork[idx]
		if res.err != nil {
			slog.Warn("vlm failed", "page", idx, "kind", work.kind, "err", res.err)
			if work.kind == "ocr" {
				textPages[idx] = "[OCR failed]"
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
		"mode", "text", "elapsed", time.Since(t0).Seconds())

	return ConvertResult{
		MDContent: strings.Join(parts, "\n\n---\n\n"),
	}, nil
}
