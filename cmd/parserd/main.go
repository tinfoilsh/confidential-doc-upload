package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/tinfoilsh/confidential-doc-upload/internal/parser"
	"github.com/tinfoilsh/confidential-doc-upload/internal/sandbox"
	"golang.org/x/sys/unix"
)

const maxDocumentBytes = 64 * 1024 * 1024

const (
	parserQueueTimeout = 5 * time.Second
	parserProbeTimeout = 10 * time.Second
)

var errParserBusy = errors.New("parser workers busy")

const (
	parserUID = uint32(65532)
	routerUID = uint32(65533)
)

func main() {
	if err := sandbox.ProtectProcess(); err != nil {
		fatal("protect parser broker: %v", err)
	}
	// Do not advertise broker readiness unless both immutable parser runtimes
	// can actually cross the sandbox/exec boundary and process a document.
	if err := probeParserRuntimes(); err != nil {
		fatal("probe parser runtimes: %v", err)
	}

	socketPath := envOr("PARSER_SOCKET", "/run/docparser/parser.sock")
	lock, err := lockSocketDirectory(socketPath)
	if err != nil {
		fatal("lock parser socket directory: %v", err)
	}
	defer lock.Close()
	if err := prepareSocket(socketPath); err != nil {
		fatal("prepare socket: %v", err)
	}
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		fatal("listen: %v", err)
	}
	// The router runs under a distinct UID in the parser's group. Group access
	// permits connect(), while the non-writable directory prevents the router
	// from replacing either the socket or the broker lock.
	if err := os.Chmod(socketPath, 0660); err != nil {
		listener.Close()
		fatal("protect socket: %v", err)
	}
	listener = &credentialListener{
		Listener: listener,
		allowedUIDs: map[uint32]struct{}{
			parserUID: {},
			routerUID: {},
		},
	}

	workerCount := boundedEnvInt("PARSER_WORKERS", 2, 1, 2)
	parserSlots := make(chan struct{}, workerCount)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /v1/extract", handleParse(parser.Extract, parserSlots))
	mux.HandleFunc("POST /v1/render", handleParse(parser.Render, parserSlots))
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       310 * time.Second,
		WriteTimeout:      310 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    8 * 1024,
	}
	slog.Info("parser broker starting", "socket", socketPath, "workers", workerCount)
	if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fatal("serve: %v", err)
	}
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	// The listener opens only after both parser runtimes pass probeParserRuntimes.
	// Their executables and libraries live on the container's read-only rootfs,
	// so broker liveness also proves the immutable runtime passed readiness.
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"status":"ok","runtimes":"probed"}`+"\n")
}

func handleParse(operation parser.Operation, parserSlots chan struct{}) http.HandlerFunc {
	return func(w http.ResponseWriter, request *http.Request) {
		filename, err := decodeFilename(request.Header.Get("X-Document-Name"))
		if err != nil {
			http.Error(w, "invalid document name", http.StatusBadRequest)
			return
		}
		dpi := 100
		if operation == parser.Render {
			dpi, err = strconv.Atoi(request.URL.Query().Get("dpi"))
			if err != nil || dpi < 20 || dpi > 600 {
				http.Error(w, "invalid dpi", http.StatusBadRequest)
				return
			}
		}
		if request.ContentLength > maxDocumentBytes {
			http.Error(w, "document too large", http.StatusRequestEntityTooLarge)
			return
		}
		// Admit work before buffering its body. This bounds both parser process
		// count and parser-side document memory to PARSER_WORKERS.
		if err := acquireParserSlot(request.Context(), parserSlots, parserQueueTimeout); err != nil {
			if errors.Is(err, errParserBusy) {
				w.Header().Set("Retry-After", strconv.Itoa(int(parserQueueTimeout/time.Second)))
				http.Error(w, "parser busy", http.StatusTooManyRequests)
			}
			return
		}
		defer func() { <-parserSlots }()

		request.Body = http.MaxBytesReader(w, request.Body, maxDocumentBytes)
		data, err := io.ReadAll(request.Body)
		if err != nil {
			http.Error(w, "document too large", http.StatusRequestEntityTooLarge)
			return
		}

		result, err := parser.Run(request.Context(), data, filename, operation, dpi)
		if err != nil {
			slog.Warn("document parser failed", "operation", operation, "err", err)
			http.Error(w, "parser failed", http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write(result)
	}
}

func acquireParserSlot(ctx context.Context, slots chan struct{}, timeout time.Duration) error {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errParserBusy
	}
}

func probeParserRuntimes() error {
	probes := []struct {
		name      string
		data      []byte
		format    string
		pageCount int
		markdown  string
	}{
		{
			name:      "00000000000000000000000000000000.pdf",
			format:    "pdf",
			pageCount: 1,
			data: []byte("%PDF-1.4\n1 0 obj\n<< /Type /Catalog /Pages 2 0 R >>\nendobj\n" +
				"2 0 obj\n<< /Type /Pages /Kids [3 0 R] /Count 1 >>\nendobj\n" +
				"3 0 obj\n<< /Type /Page /Parent 2 0 R /MediaBox [0 0 72 72] >>\nendobj\n" +
				"trailer\n<< /Root 1 0 R >>\n%%EOF\n"),
		},
		{
			name:     "00000000000000000000000000000000.txt",
			data:     []byte("parser-ready"),
			format:   "text",
			markdown: "parser-ready",
		},
	}
	for _, probe := range probes {
		ctx, cancel := context.WithTimeout(context.Background(), parserProbeTimeout)
		output, err := parser.Run(ctx, probe.data, probe.name, parser.Extract, 100)
		cancel()
		if err != nil {
			return fmt.Errorf("%s: %w", filepath.Ext(probe.name), err)
		}
		var result struct {
			Format    string `json:"format"`
			PageCount int    `json:"page_count"`
			MDContent string `json:"md_content"`
		}
		if err := json.Unmarshal(output, &result); err != nil {
			return fmt.Errorf("%s returned invalid JSON: %w", filepath.Ext(probe.name), err)
		}
		if result.Format != probe.format || result.PageCount != probe.pageCount || result.MDContent != probe.markdown {
			return fmt.Errorf("%s returned an unexpected readiness result", filepath.Ext(probe.name))
		}
	}
	return nil
}

// credentialListener authenticates Unix-socket peers at the kernel boundary.
// Filesystem mode bits remain defense in depth, but are no longer the only
// authorization check for the private parser API.
type credentialListener struct {
	net.Listener
	allowedUIDs map[uint32]struct{}
}

func (listener *credentialListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		uid, err := peerUID(connection)
		if err == nil {
			if _, allowed := listener.allowedUIDs[uid]; allowed {
				return connection, nil
			}
		}
		_ = connection.Close()
	}
}

func peerUID(connection net.Conn) (uint32, error) {
	unixConnection, ok := connection.(*net.UnixConn)
	if !ok {
		return 0, errors.New("parser listener accepted a non-Unix connection")
	}
	raw, err := unixConnection.SyscallConn()
	if err != nil {
		return 0, err
	}
	var credentials *unix.Ucred
	var socketErr error
	if err := raw.Control(func(descriptor uintptr) {
		credentials, socketErr = unix.GetsockoptUcred(int(descriptor), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if socketErr != nil {
		return 0, socketErr
	}
	return credentials.Uid, nil
}

func lockSocketDirectory(socketPath string) (*os.File, error) {
	directory := filepath.Dir(socketPath)
	if err := os.MkdirAll(directory, 0750); err != nil {
		return nil, err
	}
	if err := os.Chmod(directory, 0750); err != nil {
		return nil, err
	}
	lockPath := filepath.Join(directory, ".parserd.lock")
	descriptor, err := unix.Open(lockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	lock := os.NewFile(uintptr(descriptor), lockPath)
	if err := unix.Flock(descriptor, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("another parser broker owns %s: %w", lockPath, err)
	}
	return lock, nil
}

func prepareSocket(socketPath string) error {
	info, err := os.Lstat(socketPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refusing to remove non-socket path %s", socketPath)
	}
	return os.Remove(socketPath)
}

func decodeFilename(value string) (string, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) == 0 || len(decoded) > 255 {
		return "", errors.New("invalid filename")
	}
	name := filepath.Base(string(decoded))
	if name != string(decoded) || !validDocumentName(name) {
		return "", errors.New("invalid filename")
	}
	return name, nil
}

// The router deliberately replaces attacker-controlled names with 128 bits of
// lowercase hex plus a validated extension. Enforce that private API contract
// again at the trust boundary.
func validDocumentName(name string) bool {
	if len(name) < 34 || len(name) > 48 || name[32] != '.' {
		return false
	}
	for _, character := range name[:32] {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	for _, character := range name[33:] {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func boundedEnvInt(name string, fallback, minimum, maximum int) int {
	value, err := strconv.Atoi(os.Getenv(name))
	if err != nil || value < minimum || value > maximum {
		return fallback
	}
	return value
}

func fatal(format string, args ...any) {
	slog.Error(fmt.Sprintf(format, args...))
	os.Exit(1)
}
