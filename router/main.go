package main

import (
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
	vllmURL     = envOr("VLLM_URL", "http://localhost:8000")
	vllmModel   = envOr("VLLM_MODEL", "Qwen/Qwen3-VL-2B-Instruct")
	gemmaURL    = envOr("GEMMA_URL", "")
	gemmaModel  = envOr("GEMMA_MODEL", "gemma4-31b")
	gemmaKey    = envOr("GEMMA_KEY", "")
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
)

func init() {
	prometheus.MustRegister(metricReqs, metricDuration, metricActive, metricErrors)
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.Handle("GET /metrics", promhttp.Handler())
	mux.HandleFunc("POST /v1/convert/file", handleConvert)

	slog.Info("router starting",
		"addr", ":"+listenAddr,
		"sidecar", sidecarURL,
		"vllm", vllmURL,
		"gemma", gemmaURL)
	if err := http.ListenAndServe(":"+listenAddr, mux); err != nil {
		slog.Error("server failed", "err", err)
		os.Exit(1)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	sOK := checkHealth(sidecarURL + "/health")
	vOK := checkHealth(vllmURL + "/health")
	status := "ok"
	if !sOK {
		status = "degraded"
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"status": status, "router": true, "sidecar": sOK, "vllm": vOK,
	})
}

func handleConvert(w http.ResponseWriter, r *http.Request) {
	metricActive.Inc()
	defer metricActive.Dec()
	t0 := time.Now()

	fast := r.URL.Query().Get("fast") == "true"
	includeImages := r.URL.Query().Get("include_images") == "true"

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
			data, _ := io.ReadAll(io.LimitReader(part, int64(maxFileMB)*1024*1024+1))
			part.Close()
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
		result, err := convertFile(ctx, f.data, f.name, fast, includeImages)
		if err != nil {
			httpErr(w, 502, "processing failed", "err", err)
			return
		}

		mode := "default"
		if fast {
			mode = "fast"
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
	for _, f := range files {
		result, err := convertFile(ctx, f.data, f.name, fast, includeImages)
		if err != nil {
			httpErr(w, 502, "processing failed", "file", f.name, "err", err)
			return
		}
		docs = append(docs, result)
	}
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
