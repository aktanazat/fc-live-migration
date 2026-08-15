package hostd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/aktanazat/fc-live-migration/internal/api"
	"github.com/aktanazat/fc-live-migration/internal/sparse"
)

// resolveWritePath validates dir/name from a files/* query string,
// confirms the resulting path stays under s.snapshotsDir, creates
// dir if needed, and returns the file path to write.
func (s *Server) resolveWritePath(dir, name string) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("dir is required")
	}
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
		return "", fmt.Errorf("invalid name %q", name)
	}
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("dir %q must be absolute", dir)
	}
	cleanDir := filepath.Clean(dir)
	rel, err := filepath.Rel(s.snapshotsDir, cleanDir)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("dir %q escapes %s", dir, s.snapshotsDir)
	}
	if err := os.MkdirAll(cleanDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", cleanDir, err)
	}
	return filepath.Join(cleanDir, name), nil
}

// handleFilesBase receives a whole file, truncating any existing
// file of the same name (used for round-0 base transfers and the
// vmstate file).
func (s *Server) handleFilesBase(w http.ResponseWriter, r *http.Request) {
	path, err := s.resolveWritePath(r.URL.Query().Get("dir"), r.URL.Query().Get("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("open %s: %w", path, err))
		return
	}
	defer f.Close()
	defer r.Body.Close()

	n, err := io.Copy(f, r.Body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("write %s: %w", path, err))
		return
	}
	writeJSON(w, http.StatusOK, api.FileWriteResponse{BytesWritten: n})
}

// handleFilesExtents applies a sparse extent stream in place onto an
// existing (or newly created) file, leaving bytes outside the
// streamed extents untouched.
func (s *Server) handleFilesExtents(w http.ResponseWriter, r *http.Request) {
	path, err := s.resolveWritePath(r.URL.Query().Get("dir"), r.URL.Query().Get("name"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("open %s: %w", path, err))
		return
	}
	defer f.Close()
	defer r.Body.Close()

	n, extents, err := sparse.ApplyExtents(r.Body, f)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("apply extents to %s: %w", path, err))
		return
	}
	writeJSON(w, http.StatusOK, api.FileWriteResponse{BytesWritten: n, Extents: len(extents)})
}
