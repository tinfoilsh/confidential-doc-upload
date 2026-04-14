package pdftomd

import (
	"math"
	"sort"

	"github.com/tinfoilsh/confidential-doc-upload/internal/mupdf"
)

type HeaderMap map[int]string

// BuildHeaderMap scans all pages and builds a mapping from rounded font size
// to markdown header prefix ("# ", "## ", etc.). The most frequent font size
// is treated as body text; larger sizes become headers.
func BuildHeaderMap(pages []*mupdf.Page, bodyLimit float64) HeaderMap {
	if bodyLimit == 0 {
		bodyLimit = 12
	}

	freq := make(map[int]int)
	for _, page := range pages {
		for _, block := range page.Blocks {
			if block.Type != 0 {
				continue
			}
			for _, line := range block.Lines {
				for _, ch := range line.Chars {
					if ch.Rune <= ' ' {
						continue
					}
					sz := int(math.Round(ch.Size))
					freq[sz] += 1
				}
			}
		}
	}

	if len(freq) == 0 {
		return HeaderMap{}
	}

	// Most frequent font size = body text
	maxCount := 0
	bodySize := int(bodyLimit)
	for sz, count := range freq {
		if count > maxCount {
			maxCount = count
			bodySize = sz
		}
	}
	if float64(bodySize) < bodyLimit {
		bodySize = int(bodyLimit)
	}

	// Collect sizes larger than body, sorted descending
	var headerSizes []int
	for sz := range freq {
		if sz > bodySize {
			headerSizes = append(headerSizes, sz)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(headerSizes)))

	if len(headerSizes) > 6 {
		headerSizes = headerSizes[:6]
	}

	hm := make(HeaderMap)
	for i, sz := range headerSizes {
		prefix := ""
		for j := 0; j <= i; j++ {
			prefix += "#"
		}
		hm[sz] = prefix + " "
	}
	return hm
}

// GetHeaderPrefix returns the markdown header prefix for a given font size,
// or empty string for body text.
func (hm HeaderMap) GetHeaderPrefix(fontSize float64) string {
	sz := int(math.Round(fontSize))
	return hm[sz]
}
