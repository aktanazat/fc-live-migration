// Package hostd is the per-host migration agent: an HTTP server
// (the REST contract in internal/api) fronting a registry of
// Firecracker microVMs, plus the file-transfer and cutover machinery
// that moves a running guest between hosts in under 30ms of
// blackout.
package hostd

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aktanazat/fc-live-migration/internal/api"
)

// Server holds hostd's configuration and live VM registry.
type Server struct {
	fcBin        string
	snapshotsDir string
	runDir       string
	logger       *slog.Logger

	// peerClient is the single keep-alive HTTP client used for every
	// host-to-host file push and cutover call. Reusing it means
	// cutover never pays TCP/TLS handshake cost inside the blackout
	// window.
	peerClient *http.Client

	reg *registry
}

// Config are the knobs cmd/hostd exposes as flags.
type Config struct {
	FCBin        string
	SnapshotsDir string
	RunDir       string
	Logger       *slog.Logger
}

// NewServer builds a Server ready to serve Routes().
func NewServer(cfg Config) *Server {
	return &Server{
		fcBin:        cfg.FCBin,
		snapshotsDir: cfg.SnapshotsDir,
		runDir:       cfg.RunDir,
		logger:       cfg.Logger,
		peerClient: &http.Client{
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 4,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		reg: newRegistry(),
	}
}

// Routes returns the hostd HTTP handler implementing every endpoint
// in internal/api's package doc.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("POST /vms", s.handleCreateVM)
	mux.HandleFunc("POST /vms/prepare", s.handlePrepareVM)
	mux.HandleFunc("GET /vms/{id}", s.handleGetVM)
	mux.HandleFunc("DELETE /vms/{id}", s.handleDeleteVM)
	mux.HandleFunc("POST /vms/{id}/pause", s.handlePause)
	mux.HandleFunc("POST /vms/{id}/resume", s.handleResume)
	mux.HandleFunc("POST /vms/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /vms/{id}/push", s.handlePush)
	mux.HandleFunc("POST /vms/{id}/load", s.handleLoad)
	mux.HandleFunc("POST /vms/{id}/cutover", s.handleCutover)
	mux.HandleFunc("POST /files/base", s.handleFilesBase)
	mux.HandleFunc("POST /files/extents", s.handleFilesExtents)
	return mux
}

// KillAll force-kills every known VM's Firecracker process. Used on
// graceful shutdown.
func (s *Server) KillAll() {
	for _, vm := range s.reg.list() {
		if err := vm.FC.Kill(); err != nil {
			s.logger.Error("kill vm on shutdown", "vm", vm.ID, "err", err)
		}
	}
}

func (s *Server) vmDir(id string) string {
	return filepath.Join(s.snapshotsDir, id)
}

func (s *Server) sockPath(id string) string {
	return filepath.Join(s.runDir, id+".sock")
}

func (s *Server) logPath(id string) string {
	return filepath.Join(s.runDir, id+".log")
}

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
	}{OK: true})
}

func msSince(start time.Time) float64 {
	return float64(time.Since(start)) / float64(time.Millisecond)
}

// copyFile atomically replaces dst with a copy of src: written to a
// sibling temp file first, then renamed into place, so a concurrent
// reader never sees a torn file.
func copyFile(dst, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	tmp := dst + ".tmp"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", tmp, err)
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return fmt.Errorf("copy %s to %s: %w", src, tmp, err)
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("close %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename %s to %s: %w", tmp, dst, err)
	}
	return nil
}

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("decode request body: %w", err)
	}
	return nil
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, api.Error{Error: err.Error()})
}
