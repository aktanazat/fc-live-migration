package hostd

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aktanazat/fc-live-migration/internal/api"
)

// handlePush streams a set of local files to a peer hostd: whole
// (via /files/base) or as sparse extents (via /files/extents),
// depending on each PushFile's Sparse flag. Used for the pre-copy
// rounds; the source VM keeps running while this transfers.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.PushRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.TargetURL == "" || req.RemoteDir == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("target_url and remote_dir are required"))
		return
	}
	if _, ok := s.reg.get(id); !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("vm %s not found", id))
		return
	}

	start := time.Now()
	var totalBytes int64
	var totalExtents int
	for _, pf := range req.Files {
		n, extents, err := s.pushOne(r.Context(), req.TargetURL, req.RemoteDir, pf)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("push %s: %w", pf.Path, err))
			return
		}
		totalBytes += n
		totalExtents += extents
	}

	writeJSON(w, http.StatusOK, api.PushResponse{
		BytesSent: totalBytes,
		Extents:   totalExtents,
		Ms:        msSince(start),
	})
}

// pushOne sends one PushFile to the peer, choosing the sparse
// extents endpoint or the whole-file endpoint per pf.Sparse.
func (s *Server) pushOne(ctx context.Context, targetURL, remoteDir string, pf api.PushFile) (int64, int, error) {
	if pf.Sparse {
		return s.postExtents(ctx, filesExtentsURL(targetURL, remoteDir, pf.Name), pf.Path)
	}
	n, err := s.postFile(ctx, filesBaseURL(targetURL, remoteDir, pf.Name), pf.Path)
	return n, 0, err
}
