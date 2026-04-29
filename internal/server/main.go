package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	sidecarURL  = envOr("SIDECAR_URL", "http://localhost:5002")
	listenAddr  = envOr("ROUTER_PORT", "5000")
	maxFileMB   = envInt("MAX_FILE_SIZE_MB", 50)
	maxFiles    = envInt("MAX_FILES", 10)
	maxParts    = envInt("MAX_PARTS", 64)
	maxParallel = envInt("MAX_PARALLEL", 32)

	httpClient = &http.Client{
		Timeout:   10 * time.Minute,
		Transport: &http.Transport{MaxIdleConns: 128, MaxIdleConnsPerHost: 64, IdleConnTimeout: 90 * time.Second},
	}

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
)

func init() {
	prometheus.MustRegister(metricReqs, metricDuration, metricActive, metricErrors,
		metricPages, metricSize, metricDocType)
}

func Main() {
	initTinfoilClient()

	// Seed lastVLMSuccess with a real inference roundtrip so /health
	// reflects actual upstream connectivity (not just whether NewClient
	// returned a non-nil pointer). If this fails we still start serving —
	// /health will return 503 and the watchdog will keep retrying so the
	// service self-heals when the upstream enclave comes back.
	probeCtx, probeCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if err := vlmProbe(probeCtx); err != nil {
		slog.Error("VLM startup probe failed; /health will report degraded", "err", err)
	} else {
		slog.Info("VLM startup probe succeeded", "model", vlmModel)
	}
	probeCancel()

	go vlmWatchdog(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /v1/convert/file", handleConvert)

	slog.Info("router starting",
		"addr", ":"+listenAddr,
		"sidecar", sidecarURL,
		"vlm_model", vlmModel)
	srv := &http.Server{
		Addr:        ":" + listenAddr,
		Handler:     mux,
		ReadTimeout: 5 * time.Minute,
		IdleTimeout: 120 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

// handleHealth returns 200 only when every dependency is currently usable.
//
// Previously this returned 200 with `"status":"degraded"` in the body, so
// upstream probes (Betterstack, k8s readiness, load balancer health checks)
// considered the service healthy even when the VLM was completely
// unreachable — most probes only key off the HTTP status code. The vlmOK
// check also used to be a nil-pointer check, which couldn't catch SDK
// connection loss, failed cert rotation, or stale-enclave conditions.
//
// vlmHealthy() reads the timestamp of the last successful inference (real
// traffic OR background probe), so this endpoint reflects current upstream
// connectivity within vlmHealthyWindow.
func handleHealth(w http.ResponseWriter, r *http.Request) {
	sOK := checkHealth(sidecarURL + "/health")
	vlmOK := vlmHealthy()
	status := "ok"
	code := http.StatusOK
	if !sOK || !vlmOK {
		status = "degraded"
		code = http.StatusServiceUnavailable
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":     status,
		"router":     true,
		"sidecar":    sOK,
		"vlm":        vlmOK,
		"vlm_model":  vlmModel,
		"checked_at": time.Now().UTC().Format(time.RFC3339),
	})
}

func handleConvert(w http.ResponseWriter, r *http.Request) {
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

	r.Body = http.MaxBytesReader(w, r.Body, int64(maxFiles*maxFileMB+10)*1024*1024)
	ct := r.Header.Get("Content-Type")
	mediaType, params, err := mime.ParseMediaType(ct)
	if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
		httpErr(w, 400, "expected multipart/form-data")
		return
	}

	reader := multipart.NewReader(r.Body, params["boundary"])
	type uploadedFile struct {
		name string
		data []byte
	}
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
			if len(files) >= maxFiles {
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
			name := randomName(part.FileName())
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

	if len(files) == 1 {
		f := files[0]
		result, err := convertFile(ctx, f.data, f.name, mode)
		if err != nil {
			slog.Error("convert failed", "err", err)
			httpErr(w, 502, "processing failed")
			return
		}

		metricReqs.WithLabelValues("pdf", mode).Inc()
		metricDuration.WithLabelValues("pdf", mode).Observe(time.Since(t0).Seconds())

		json.NewEncoder(w).Encode(map[string]any{
			"document":        result,
			"status":          "success",
			"processing_time": time.Since(t0).Seconds(),
		})
		return
	}

	var docs []ConvertResult
	for i, f := range files {
		result, err := convertFile(ctx, f.data, f.name, mode)
		if err != nil {
			slog.Error("convert failed", "file_index", i, "err", err)
			httpErr(w, 502, "processing failed")
			return
		}
		docs = append(docs, result)
	}
	metricReqs.WithLabelValues("pdf", mode).Inc()
	metricDuration.WithLabelValues("pdf", mode).Observe(time.Since(t0).Seconds())

	json.NewEncoder(w).Encode(map[string]any{
		"documents":       docs,
		"status":          "success",
		"processing_time": time.Since(t0).Seconds(),
	})
}

// --- helpers ---

func randomName(orig string) string {
	var b [16]byte
	rand.Read(b[:])
	ext := filepath.Ext(orig)
	if ext == "" {
		ext = ".pdf"
	}
	return hex.EncodeToString(b[:]) + ext
}

func checkHealth(url string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	r, err := c.Get(url)
	if err != nil {
		return false
	}
	r.Body.Close()
	return r.StatusCode == 200
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
