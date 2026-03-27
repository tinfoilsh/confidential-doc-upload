package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/bits"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"


	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	doclingURL = envOr("DOCLING_URL", "http://localhost:5001")
	vllmURL    = envOr("VLLM_URL", "http://localhost:8000")
	vllmModel  = envOr("VLLM_MODEL", "Qwen/Qwen3-VL-2B-Instruct")
	listenAddr = envOr("ROUTER_PORT", "5000")
	chunkPages = envInt("CHUNK_PAGES", 3)
	splitMin   = envInt("SPLIT_THRESHOLD", 2)
	maxFileMB  = envInt("MAX_FILE_SIZE_MB", 50)
	maxChunks  = envInt("MAX_CONCURRENT_CHUNKS", 32)

	chunkSem   chan struct{}

	httpClient = &http.Client{
		Timeout: 10 * time.Minute,
		Transport: &http.Transport{
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 64,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	reqTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_requests_total",
		Help: "Total requests by PDF type and routing strategy.",
	}, []string{"pdf_type", "strategy"})
	reqDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "router_request_duration_seconds",
		Help:    "End-to-end request duration including all chunk processing.",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
	}, []string{"pdf_type", "strategy"})
	activeReqs = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "router_active_requests",
		Help: "Number of requests currently being processed.",
	})
	docSizeBytes = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "router_document_size_bytes",
		Help:    "Uploaded document size in bytes.",
		Buckets: prometheus.ExponentialBuckets(1024, 4, 10), // 1KB to 256MB
	}, []string{"pdf_type"})
	docPages = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "router_document_pages",
		Help:    "Number of pages in uploaded documents (coarse buckets).",
		Buckets: []float64{1, 10, 50, 100, 500, 1000},
	}, []string{"pdf_type"})
	reqErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_errors_total",
		Help: "Total errors by type.",
	}, []string{"error_type"})
)

func init() {
	prometheus.MustRegister(reqTotal, reqDuration, activeReqs, docSizeBytes, docPages, reqErrors)
	chunkSem = make(chan struct{}, maxChunks)
}

type pdfType string

const (
	pdfBornDigital pdfType = "born_digital"
	pdfScanned     pdfType = "scanned"
)

func proxyTo(target string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		resp, err := http.Get(target)
		if err != nil {
			http.Error(w, "backend unavailable", http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}
}

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("GET /health/docling", proxyTo(doclingURL+"/health"))
	mux.HandleFunc("GET /health/vllm", proxyTo(vllmURL+"/health"))

	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("GET /metrics/docling", proxyTo(doclingURL+"/metrics"))
	mux.HandleFunc("GET /metrics/vllm", proxyTo(vllmURL+"/metrics"))

	mux.HandleFunc("POST /v1/convert/file", handleConvert)

	slog.Info("router starting", "addr", ":"+listenAddr, "docling", doclingURL, "vllm", vllmURL)
	if err := http.ListenAndServe(":"+listenAddr, mux); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	dOK := checkHealth(doclingURL + "/health")
	vOK := checkHealth(vllmURL + "/health")
	status := "ok"
	if !dOK {
		status = "degraded"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": status, "router": true, "docling": dOK, "vllm": vOK,
	})
}

// picDescConfig returns the picture_description_api JSON for VLM figure enrichment.
func picDescConfig() string {
	cfg := map[string]any{
		"url":         vllmURL + "/v1/chat/completions",
		"params":      map[string]any{"model": vllmModel, "max_completion_tokens": 1000},
		"timeout":     60.0,
		"concurrency": 8,
		"prompt":      "Describe this image in detail. If it contains a table, extract it as markdown. If it contains a chart, describe the data and trends.",
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

// vlmPipelineConfig returns the vlm_pipeline_custom_config JSON for scanned docs.
func vlmPipelineConfig() string {
	cfg := map[string]any{
		"engine_options": map[string]any{
			"engine_type": "api",
			"url":         vllmURL + "/v1/chat/completions",
			"params":      map[string]any{"model": vllmModel, "max_tokens": 16384},
			"timeout":     120.0,
			"concurrency": 16,
		},
		"model_spec": map[string]any{
			"name":            vllmModel,
			"default_repo_id": vllmModel,
			"prompt":          "Convert this page to markdown. Do not miss any text and only output the bare markdown!",
			"response_format": "markdown",
			"max_new_tokens":  16384,
		},
		"scale":      2.0,
		"batch_size": 1,
	}
	b, _ := json.Marshal(cfg)
	return string(b)
}

func handleConvert(w http.ResponseWriter, r *http.Request) {
	activeReqs.Inc()
	defer activeReqs.Dec()
	t0 := time.Now()

	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		httpErr(w, http.StatusBadRequest, "expected multipart/form-data")
		return
	}

	reader := multipart.NewReader(r.Body, params["boundary"])
	var pdfData []byte
	var pdfFilename string
	var clientFields []fieldPair

	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			httpErr(w, http.StatusBadRequest, "failed to parse multipart")
			return
		}
		name := part.FormName()
		if name == "files" {
			if pdfData != nil {
				part.Close()
				continue
			}
			pdfFilename = part.FileName()
			data, err := io.ReadAll(io.LimitReader(part, int64(maxFileMB)*1024*1024+1))
			part.Close()
			if err != nil || len(data) > maxFileMB*1024*1024 {
				httpErr(w, http.StatusRequestEntityTooLarge,
					fmt.Sprintf("file exceeds %dMB limit", maxFileMB))
				return
			}
			pdfData = data
		} else {
			val, _ := io.ReadAll(io.LimitReader(part, 1024*1024))
			part.Close()
			clientFields = append(clientFields, fieldPair{name, string(val)})
		}
	}

	if pdfData == nil {
		httpErr(w, http.StatusBadRequest, "no file uploaded")
		return
	}

	// If client set pipeline or do_ocr, respect their choice
	if hasField(clientFields, "pipeline") || hasField(clientFields, "do_ocr") {
		body, _, err := postMultipartStreaming(pdfData, pdfFilename, clientFields)
		if err != nil {
			httpErr(w, http.StatusBadGateway, "backend error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
		return
	}

	pt, pages := classifyPDFRaw(pdfData)

	// Two paths:
	//   born_digital → standard pipeline, no OCR, VLM describes figures
	//   scanned      → VLM pipeline (reads page images directly)
	var strategy string
	var routeFields []fieldPair

	describeImages := r.URL.Query().Get("describe_images") == "true"

	if pt == pdfBornDigital {
		strategy = "standard"
		routeFields = []fieldPair{
			{"do_ocr", "false"},
		}
		if describeImages {
			strategy = "standard+pics"
			routeFields = append(routeFields,
				fieldPair{"do_picture_description", "true"},
				fieldPair{"do_picture_classification", "true"},
				fieldPair{"picture_description_api", picDescConfig()},
			)
		}
	} else {
		strategy = "vlm"
		routeFields = []fieldPair{
			{"pipeline", "vlm"},
			{"vlm_pipeline_custom_config", vlmPipelineConfig()},
		}
	}

	allFields := append(clientFields, routeFields...)
	shouldSplit := pages > splitMin

	slog.Info("request",
		"type", pt, "pages", pages,
		"strategy", strategy, "split", shouldSplit)
	reqTotal.WithLabelValues(string(pt), strategy).Inc()
	docSizeBytes.WithLabelValues(string(pt)).Observe(float64(nextPow2(len(pdfData))))
	docPages.WithLabelValues(string(pt)).Observe(float64(pages))

	if shouldSplit {
		nChunks := int(math.Ceil(float64(pages) / float64(chunkPages)))
		type chunkResult struct {
			idx  int
			body []byte
			err  error
		}
		results := make([]chunkResult, nChunks)
		var wg sync.WaitGroup

		for i := 0; i < nChunks; i++ {
			wg.Add(1)
			go func(idx int) {
				defer wg.Done()
				chunkSem <- struct{}{}
				defer func() { <-chunkSem }()

				start := idx*chunkPages + 1
				end := (idx + 1) * chunkPages
				if end > pages {
					end = pages
				}

				chunkFields := make([]fieldPair, len(allFields), len(allFields)+2)
				copy(chunkFields, allFields)
				chunkFields = append(chunkFields,
					fieldPair{"page_range", strconv.Itoa(start)},
					fieldPair{"page_range", strconv.Itoa(end)},
				)

				body, _, err := postMultipartStreaming(pdfData, pdfFilename, chunkFields)
				results[idx] = chunkResult{idx, body, err}
			}(i)
		}
		wg.Wait()

		var mds []string
		for _, cr := range results {
			if cr.err != nil {
				httpErr(w, http.StatusBadGateway, "chunk processing failed")
				return
			}
			var resp doclingResponse
			if err := json.Unmarshal(cr.body, &resp); err != nil {
				httpErr(w, http.StatusBadGateway, "invalid chunk response")
				return
			}
			mds = append(mds, resp.Document.MDContent)
		}

		merged := doclingResponse{
			Document: doclingDocument{
				Filename:  pdfFilename,
				MDContent: strings.Join(mds, "\n\n"),
			},
			Status:         "success",
			ProcessingTime: time.Since(t0).Seconds(),
		}

		elapsed := time.Since(t0).Seconds()
		reqDuration.WithLabelValues(string(pt), strategy).Observe(elapsed)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Router-Type", string(pt))
		w.Header().Set("X-Router-Strategy", strategy)
		w.Header().Set("X-Router-Chunks", strconv.Itoa(nChunks))
		json.NewEncoder(w).Encode(merged)
	} else {
		body, _, err := postMultipartStreaming(pdfData, pdfFilename, allFields)
		if err != nil {
			httpErr(w, http.StatusBadGateway, "backend error")
			return
		}

		elapsed := time.Since(t0).Seconds()
		reqDuration.WithLabelValues(string(pt), strategy).Observe(elapsed)

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Router-Type", string(pt))
		w.Header().Set("X-Router-Strategy", strategy)
		w.Write(body)
	}
}

func checkHealth(url string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func httpErr(w http.ResponseWriter, code int, msg string) {
	slog.Error("request error", "code", code, "msg", msg)
	reqErrors.WithLabelValues(strconv.Itoa(code)).Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

type fieldPair struct{ key, value string }

type doclingResponse struct {
	Document       doclingDocument `json:"document"`
	Status         string          `json:"status"`
	Errors         []any           `json:"errors"`
	ProcessingTime float64         `json:"processing_time"`
}

type doclingDocument struct {
	Filename  string `json:"filename"`
	MDContent string `json:"md_content"`
}

func hasField(fields []fieldPair, key string) bool {
	for _, f := range fields {
		if f.key == key {
			return true
		}
	}
	return false
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func nextPow2(n int) int {
	if n <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(n-1))
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
