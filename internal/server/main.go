package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/tinfoilsh/confidential-doc-upload/internal/sandbox"
)

var (
	listenAddr  = envOr("ROUTER_PORT", "5000")
	maxFileMB   = boundedEnvInt("MAX_FILE_SIZE_MB", 50, 1, 64)
	maxFiles    = boundedEnvInt("MAX_FILES", 2, 1, 2)
	maxParts    = boundedEnvInt("MAX_PARTS", 64, 1, 128)
	maxParallel = boundedEnvInt("MAX_PARALLEL", 8, 1, 32)
	// Combined with the two-file batch limit, the 256 MiB parser-output ceiling,
	// and the one-file image-mode limit, four requests keep retained results and
	// buffered uploads below the router's 6 GiB cgroup limit. Higher admission
	// provides no parser throughput benefit because the broker admits two children.
	maxActive   = boundedEnvInt("MAX_ACTIVE_REQUESTS", 4, 1, 4)
	requestGate = make(chan struct{}, maxActive)
	// Multi-file requests may use two workers, but all requests together can
	// never exceed the pre-existing MAX_ACTIVE_REQUESTS work ceiling.
	documentGate = make(chan struct{}, maxActive)
	// One service-wide VLM budget prevents concurrent files or requests from
	// multiplying the configured outbound fan-out.
	vlmGate = make(chan struct{}, maxParallel)

	metricReqs     = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "router_requests_total"}, []string{"format", "mode"})
	metricDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "router_duration_seconds", Buckets: prometheus.ExponentialBuckets(0.01, 2, 14)}, []string{"format", "mode"})
	metricActive   = prometheus.NewGauge(prometheus.GaugeOpts{Name: "router_active_requests"})
	metricErrors   = prometheus.NewCounterVec(prometheus.CounterOpts{Name: "router_errors_total"}, []string{"type"})

	// Coarse-grained buckets to prevent document fingerprinting via exact values
	metricPages = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "router_document_pages",
		Help:    "Number of pages per document (bucketed)",
		Buckets: []float64{1, 2, 5, 10, 20, 50, 100, 200},
	})
	metricSize = prometheus.NewHistogram(prometheus.HistogramOpts{
		Name:    "router_document_size_bytes",
		Help:    "Document size in bytes (bucketed)",
		Buckets: prometheus.ExponentialBuckets(1024, 4, 8), // 1KB, 4KB, 16KB, 64KB, 256KB, 1MB, 4MB, 16MB
	})
	metricDocType = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_documents_total",
		Help: "Documents processed by type",
	}, []string{"doc_type"}) // "born_digital", "scanned", "mixed"

	// VLM observability. result: success|empty|transport|auth|server|client.
	metricVLMCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_vlm_calls_total",
		Help: "VLM calls by kind and outcome",
	}, []string{"kind", "result"})
	metricVLMDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "router_vlm_duration_seconds",
		Help:    "VLM call latency by kind",
		Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
	}, []string{"kind"})
	metricVLMHealthy = prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "router_vlm_healthy",
		Help: "1 if VLM is currently healthy, 0 otherwise",
	}, func() float64 {
		if vlmHealth.Load() {
			return 1
		}
		return 0
	})
	metricVLMReinits = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "router_vlm_reinits_total",
		Help: "Tinfoil SDK re-init attempts after persistent unhealth",
	}, []string{"result"}) // success | error
)

type uploadedFile struct {
	name string
	data []byte
}

func init() {
	prometheus.MustRegister(metricReqs, metricDuration, metricActive, metricErrors,
		metricPages, metricSize, metricDocType,
		metricVLMCalls, metricVLMDuration, metricVLMHealthy, metricVLMReinits)
}

func Main() {
	if err := sandbox.ProtectProcess(); err != nil {
		slog.Error("failed to protect router process", "err", err)
		os.Exit(1)
	}
	initTinfoilClient()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /v1/convert/file", handleConvert)

	slog.Info("router starting",
		"addr", ":"+listenAddr,
		"parser_socket", parserSocketPath,
		"vlm_model", vlmModel)
	srv := &http.Server{
		Addr:              ":" + listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       5 * time.Minute,
		WriteTimeout:      15 * time.Minute,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    32 * 1024,
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	pOK := parserHealthy()
	vlmOK := vlmHealthy()
	status, code := healthStatus(pOK, vlmOK)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"status": status, "router": true, "parser": pOK, "vlm": vlmOK,
	})
}

// VLM is an optional dependency for raw, images, and born-digital document
// paths. Report it as degraded without failing core readiness; the private
// parser is required for every supported conversion and remains fail-closed.
func healthStatus(parserOK, vlmOK bool) (string, int) {
	if !parserOK {
		return "unavailable", http.StatusServiceUnavailable
	}
	if !vlmOK {
		return "degraded", http.StatusOK
	}
	return "ok", http.StatusOK
}

func handleConvert(w http.ResponseWriter, r *http.Request) {
	select {
	case requestGate <- struct{}{}:
		defer func() { <-requestGate }()
	default:
		httpErr(w, http.StatusTooManyRequests, "too many active requests")
		return
	}
	metricActive.Inc()
	defer metricActive.Dec()
	t0 := time.Now()

	mode := r.URL.Query().Get("mode")
	if mode == "" {
		mode = "text"
	}
	if mode != "text" && mode != "vision" && mode != "images" && mode != "raw" && mode != "vlm" {
		httpErr(w, 400, "invalid mode: must be 'text', 'vision', 'images', 'raw', or 'vlm'")
		return
	}

	fileLimit := requestFileLimit(mode)
	r.Body = http.MaxBytesReader(w, r.Body, int64(fileLimit*maxFileMB+10)*1024*1024)
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		httpErr(w, 400, "expected multipart/form-data")
		return
	}

	reader := multipart.NewReader(r.Body, params["boundary"])
	var files []uploadedFile

	for partCount := 0; ; {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			httpErr(w, 400, "bad multipart")
			return
		}
		partCount++
		if partCount > maxParts {
			part.Close()
			httpErr(w, 400, "too many parts")
			return
		}
		if part.FormName() == "files" {
			originalName := part.FileName()
			if len(files) >= fileLimit {
				part.Close()
				httpErr(w, 400, "too many files")
				return
			}
			data, err := io.ReadAll(io.LimitReader(part, int64(maxFileMB)*1024*1024+1))
			part.Close()
			if err != nil {
				httpErr(w, 400, "failed to read file")
				return
			}
			if len(data) > maxFileMB*1024*1024 {
				httpErr(w, 413, "file too large")
				return
			}
			name, err := randomName(originalName)
			if err != nil {
				httpErr(w, http.StatusInternalServerError, "failed to create document identifier")
				return
			}
			files = append(files, uploadedFile{name, data})
		} else {
			part.Close()
		}
	}

	if len(files) == 0 {
		httpErr(w, 400, "no file uploaded")
		return
	}

	ctx := r.Context()
	w.Header().Set("Content-Type", "application/json")

	docs, err := convertUploadedFiles(ctx, files, mode)
	if err != nil {
		slog.Error("convert failed", "err", err)
		if writeParserBackpressure(w, err) {
			return
		}
		httpErr(w, 502, "processing failed")
		return
	}
	metricReqs.WithLabelValues("pdf", mode).Inc()
	metricDuration.WithLabelValues("pdf", mode).Observe(time.Since(t0).Seconds())

	if len(docs) == 1 {
		json.NewEncoder(w).Encode(map[string]any{
			"document":        docs[0],
			"status":          "success",
			"processing_time": time.Since(t0).Seconds(),
		})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"documents":       docs,
		"status":          "success",
		"processing_time": time.Since(t0).Seconds(),
	})
}

func writeParserBackpressure(w http.ResponseWriter, err error) bool {
	var responseError *parserResponseError
	if !errors.As(err, &responseError) || responseError.StatusCode != http.StatusTooManyRequests {
		return false
	}
	if responseError.RetryAfterSeconds > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(responseError.RetryAfterSeconds))
	}
	httpErr(w, http.StatusTooManyRequests, "parser busy")
	return true
}

func convertUploadedFiles(ctx context.Context, files []uploadedFile, mode string) ([]ConvertResult, error) {
	return convertUploadedFilesWith(ctx, files, mode, convertFile)
}

type documentConverter func(context.Context, []byte, string, string) (ConvertResult, error)

func convertUploadedFilesWith(ctx context.Context, files []uploadedFile, mode string, convert documentConverter) ([]ConvertResult, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make([]ConvertResult, len(files))
	jobs := make(chan int, len(files))
	for index := range files {
		jobs <- index
	}
	close(jobs)

	workers := min(2, len(files))
	var wait sync.WaitGroup
	var fail sync.Once
	var firstErr error
	for range workers {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					select {
					case documentGate <- struct{}{}:
					case <-ctx.Done():
						return
					}
					file := files[index]
					result, err := convert(ctx, file.data, file.name, mode)
					<-documentGate
					if err != nil {
						fail.Do(func() {
							firstErr = fmt.Errorf("file %d: %w", index, err)
							cancel()
						})
						return
					}
					results[index] = result
				}
			}
		}()
	}
	wait.Wait()
	if firstErr == nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return results, firstErr
}

// --- helpers ---

func randomName(orig string) (string, error) {
	return randomNameFrom(rand.Reader, orig)
}

func randomNameFrom(random io.Reader, orig string) (string, error) {
	var b [16]byte
	if _, err := io.ReadFull(random, b[:]); err != nil {
		return "", fmt.Errorf("read document identifier entropy: %w", err)
	}
	ext := strings.ToLower(filepath.Ext(orig))
	if ext == "" {
		ext = ".pdf"
	} else if !safeExtension(ext) {
		ext = ".bin"
	}
	return hex.EncodeToString(b[:]) + ext, nil
}

func safeExtension(extension string) bool {
	if len(extension) < 2 || len(extension) > 16 || extension[0] != '.' {
		return false
	}
	for _, character := range extension[1:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func requestFileLimit(mode string) int {
	// Image responses retain base64 page data until JSON encoding completes.
	// A single-file cap, combined with the 256 MiB parser-output ceiling and
	// four-request admission limit, bounds retained image results to 1 GiB.
	if mode == "images" {
		return 1
	}
	return maxFiles
}

func httpErr(w http.ResponseWriter, code int, msg string, attrs ...any) {
	slog.Error("request error", append([]any{"code", code, "msg", msg}, attrs...)...)
	metricErrors.WithLabelValues(strconv.Itoa(code)).Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func envOr(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return d
}

func boundedEnvInt(name string, fallback, minimum, maximum int) int {
	value := envInt(name, fallback)
	if value < minimum || value > maximum {
		return fallback
	}
	return value
}
