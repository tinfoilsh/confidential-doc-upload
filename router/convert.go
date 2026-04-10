package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

type PageImage struct {
	Page  int    `json:"page"`
	Image string `json:"image"`
}

type ConvertResult struct {
	MDContent string      `json:"md_content"`
	Filename  string      `json:"filename"`
	Pages     []PageImage `json:"pages,omitempty"`
}

func convertFile(ctx context.Context, data []byte, filename string, fast bool, includeImages bool) (ConvertResult, error) {
	t0 := time.Now()

	ext := sidecarExtract
	_ = ext
	extracted, err := sidecarExtract(ctx, data, filename)
	if err != nil {
		return ConvertResult{}, fmt.Errorf("extract: %w", err)
	}

	// Non-PDF: direct markdown, no VLM needed
	if extracted.Format != "pdf" && extracted.Format != "image" {
		slog.Info("processed", "file", filename, "format", extracted.Format,
			"elapsed", time.Since(t0).Seconds())
		return ConvertResult{
			MDContent: extracted.MDContent,
			Filename:  filename,
		}, nil
	}

	// Image file: VLM OCR
	if extracted.Format == "image" {
		rendered, err := sidecarRender(ctx, data, filename, 100)
		if err != nil {
			return ConvertResult{}, fmt.Errorf("render image: %w", err)
		}
		if len(rendered.Pages) == 0 {
			return ConvertResult{Filename: filename}, nil
		}
		md, err := vlmFullPageOCR(ctx, rendered.Pages[0].Image)
		if err != nil {
			return ConvertResult{}, fmt.Errorf("vlm image: %w", err)
		}
		result := ConvertResult{MDContent: md, Filename: filename}
		if includeImages {
			result.Pages = []PageImage{{Page: 1, Image: rendered.Pages[0].Image}}
		}
		return result, nil
	}

	// PDF processing
	nPages := len(extracted.Pages)

	// Classify pages
	var scannedIdxs []int
	textPages := make(map[int]string)
	for _, p := range extracted.Pages {
		if p.IsScanned {
			scannedIdxs = append(scannedIdxs, p.Page)
		} else {
			textPages[p.Page] = p.Text
		}
	}

	slog.Info("classified",
		"file", filename, "pages", nPages,
		"scanned", len(scannedIdxs),
		"born_digital", len(textPages))

	// Render pages once (reused for scanned OCR, visual extraction, and include_images)
	needsRender := len(scannedIdxs) > 0 || (!fast && !includeImages) || includeImages
	var rendered RenderResult
	if needsRender {
		var err error
		rendered, err = sidecarRender(ctx, data, filename, 100)
		if err != nil {
			return ConvertResult{}, fmt.Errorf("render: %w", err)
		}
	}

	renderedImages := make(map[int]string)
	for _, rp := range rendered.Pages {
		renderedImages[rp.Page] = rp.Image
	}

	// Scanned pages: Qwen full-page OCR
	if len(scannedIdxs) > 0 {
		scannedImages := make(map[int]string)
		for _, idx := range scannedIdxs {
			if img, ok := renderedImages[idx]; ok {
				scannedImages[idx] = img
			}
		}

		ocrResults, err := vlmParallel(ctx, scannedImages, vlmFullPageOCR)
		if err != nil {
			return ConvertResult{}, fmt.Errorf("vlm ocr: %w", err)
		}
		for idx, md := range ocrResults {
			textPages[idx] = md
		}
	}

	// Visual element extraction (unless fast mode or include_images mode)
	visualDescs := make(map[int]string)
	if !fast && !includeImages {
		bdImages := make(map[int]string)
		for _, p := range extracted.Pages {
			if !p.IsScanned {
				if img, ok := renderedImages[p.Page]; ok {
					bdImages[p.Page] = img
				}
			}
		}

		if len(bdImages) > 0 {
			descResults, err := vlmParallel(ctx, bdImages, vlmVisualExtract)
			if err != nil {
				slog.Warn("visual extraction failed, continuing without",
					"err", err)
			} else {
				for idx, desc := range descResults {
					d := strings.TrimSpace(desc)
					if !strings.HasPrefix(strings.ToUpper(d), "NONE") && len(d) > 10 {
						visualDescs[idx] = d
					}
				}
			}
		}
	}

	// Merge pages in order
	var parts []string
	for i := 1; i <= nPages; i++ {
		md := textPages[i]
		if desc, ok := visualDescs[i]; ok {
			md += "\n\n## Visual Elements\n\n" + desc
		}
		parts = append(parts, md)
	}
	mdContent := strings.Join(parts, "\n\n---\n\n")

	// Optionally include page images (reuse already-rendered images)
	var pageImages []PageImage
	if includeImages {
		for _, rp := range rendered.Pages {
			pageImages = append(pageImages, PageImage{
				Page:  rp.Page,
				Image: rp.Image,
			})
		}
	}

	slog.Info("processed",
		"file", filename, "pages", nPages,
		"scanned", len(scannedIdxs),
		"visual_descs", len(visualDescs),
		"fast", fast,
		"include_images", includeImages,
		"elapsed", time.Since(t0).Seconds())

	return ConvertResult{
		MDContent: mdContent,
		Filename:  filename,
		Pages:     pageImages,
	}, nil
}
