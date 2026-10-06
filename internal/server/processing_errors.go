package server

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/tinfoilsh/confidential-doc-upload/internal/processing"
)

var metricProcessingErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "router_document_processing_errors_total",
	Help: "Document conversion failures by privacy-safe error code",
}, []string{"code"})

func init() {
	prometheus.MustRegister(metricProcessingErrors)
}

func writeProcessingError(w http.ResponseWriter, status int, code processing.Code) {
	code = processing.Normalize(code)
	message := processing.Message(code)
	errorType := "server_error"
	if status == http.StatusTooManyRequests {
		errorType = "rate_limit_error"
	}
	slog.Error("document processing failed", "status", status, "code", code)
	metricErrors.WithLabelValues(strconv.Itoa(status)).Inc()
	metricProcessingErrors.WithLabelValues(string(code)).Inc()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The flat OpenAI envelope survives model-router normalization. The legacy
	// error string remains available to clients that consume it directly.
	_ = json.NewEncoder(w).Encode(struct {
		Error   string `json:"error"`
		Object  string `json:"object"`
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
		Param   any    `json:"param"`
	}{message, "error", message, errorType, string(code), nil})
}
