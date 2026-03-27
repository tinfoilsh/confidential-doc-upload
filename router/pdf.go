package main

import (
	"regexp"
	"strconv"
)

// PDF classification without third-party dependencies.
//
// Born-digital PDFs embed fonts for text rendering; their cross-reference
// tables contain /BaseFont entries (a required key per PDF spec §9.6.2 for
// every font dictionary). Scanned PDFs consist of page-sized images with
// /Subtype /Image XObjects and no font resources.
//
// We count /BaseFont occurrences (reliable — this key only appears in font
// dictionaries, never in content streams or compressed data) and /Subtype
// /Image occurrences to classify the document.

var (
	// /BaseFont is required in every PDF font dictionary (spec §9.6.2).
	// It never appears in content streams, even when uncompressed.
	baseFontRe = regexp.MustCompile(`/BaseFont\s`)

	// /Subtype /Image identifies image XObjects (spec §8.9.5).
	imageSubtypeRe = regexp.MustCompile(`/Subtype\s*/Image`)

	// /Count N in the page tree root gives total page count (spec §7.7.3).
	pageCountRe = regexp.MustCompile(`/Count\s+(\d+)`)

	// /Type /Page but NOT /Type /Pages — matches individual page objects.
	pageObjRe = regexp.MustCompile(`/Type\s*/Page[^s]`)
)

// classifyPDFRaw determines if a PDF is born-digital (has embedded fonts),
// scanned (only images, no fonts), or mixed. It also returns the page count.
func classifyPDFRaw(data []byte) (pdfType, int) {
	if len(data) < 5 || string(data[:5]) != "%PDF-" {
		return pdfScanned, 0
	}

	pages := extractPageCount(data)
	if pages == 0 {
		return pdfScanned, 0
	}

	fontCount := len(baseFontRe.FindAll(data, -1))
	imageCount := len(imageSubtypeRe.FindAll(data, -1))

	switch {
	case fontCount > 0 && imageCount == 0:
		return pdfBornDigital, pages
	case fontCount == 0 && imageCount > 0:
		return pdfScanned, pages
	case fontCount > 0 && imageCount > 0:
		// Has both fonts and images — born-digital with embedded figures
		return pdfBornDigital, pages
	default:
		return pdfScanned, pages
	}
}

// extractPageCount finds the highest /Count value in the PDF, which
// corresponds to the root Pages node's total page count.
func extractPageCount(data []byte) int {
	matches := pageCountRe.FindAllSubmatch(data, -1)
	maxCount := 0
	for _, m := range matches {
		if len(m) > 1 {
			if n, err := strconv.Atoi(string(m[1])); err == nil && n > maxCount {
				maxCount = n
			}
		}
	}
	if maxCount > 0 {
		return maxCount
	}
	return len(pageObjRe.FindAll(data, -1))
}
