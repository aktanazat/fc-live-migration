package hostd

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sync"
	"time"

	"github.com/aktanazat/fc-live-migration/internal/api"
	"github.com/aktanazat/fc-live-migration/internal/fc"
)

// handleCutover runs the final migration phase entirely on the
// source hostd: pause, final diff snapshot, push the diff extents
// and vmstate to the peer, then tell the peer to load-and-resume.
// Everything from pause to the remote resume acknowledgment counts
// toward BlackoutMs, so every peer URL and local path is resolved
// before the pause, and no logging happens inside the window.
func (s *Server) handleCutover(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.CutoverRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.TargetURL == "" || req.TargetID == "" || req.RemoteDir == "" || req.RemoteMemName == "" || req.LocalDir == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("target_url, target_id, remote_dir, remote_mem_name, and local_dir are required"))
		return
	}

	vm, ok := s.reg.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("vm %s not found", id))
		return
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.State != api.StateRunning {
		writeError(w, http.StatusConflict, fmt.Errorf("vm %s is %s, not running", id, vm.State))
		return
	}

	if err := os.MkdirAll(req.LocalDir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("mkdir %s: %w", req.LocalDir, err))
		return
	}
	statePath := filepath.Join(req.LocalDir, snapshotStateName)
	memPath := filepath.Join(req.LocalDir, snapshotMemName)

	// Pre-resolve every peer URL and remote path so the blackout
	// window does zero string building or DNS/lookup work.
	extentsURL := filesExtentsURL(req.TargetURL, req.RemoteDir, req.RemoteMemName)
	stateURL := filesBaseURL(req.TargetURL, req.RemoteDir, snapshotStateName)
	loadURL := req.TargetURL + "/vms/" + req.TargetID + "/load"
	loadReq := api.LoadRequest{
		StatePath:       path.Join(req.RemoteDir, snapshotStateName),
		MemPath:         path.Join(req.RemoteDir, req.RemoteMemName),
		TrackDirtyPages: vm.TrackDirtyPages,
		Resume:          true,
	}

	t0 := time.Now()

	pauseStart := time.Now()
	if err := vm.FC.Pause(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("pause: %w", err))
		return
	}
	pauseMs := msSince(pauseStart)
	vm.State = api.StatePaused

	snapStart := time.Now()
	if err := vm.FC.CreateSnapshot(fc.SnapshotDiff, statePath, memPath); err != nil {
		s.cutoverRollback(vm, fmt.Errorf("diff snapshot: %w", err), w)
		return
	}
	snapMs := msSince(snapStart)

	pushStart := time.Now()
	diffBytes, extentCount, err := s.pushCutoverFiles(r.Context(), extentsURL, stateURL, memPath, statePath)
	if err != nil {
		s.cutoverRollback(vm, fmt.Errorf("push cutover files: %w", err), w)
		return
	}
	pushMs := msSince(pushStart)

	loadStart := time.Now()
	if err := s.remoteLoad(r.Context(), loadURL, loadReq); err != nil {
		s.cutoverRollback(vm, fmt.Errorf("remote load: %w", err), w)
		return
	}
	loadMs := msSince(loadStart)
	blackoutMs := msSince(t0)

	// The guest is now running on the target; this hostd's copy stays
	// paused (not resumed) until the orchestrator issues DELETE.
	writeJSON(w, http.StatusOK, api.CutoverResponse{
		PauseMs:    pauseMs,
		SnapMs:     snapMs,
		PushMs:     pushMs,
		LoadMs:     loadMs,
		BlackoutMs: blackoutMs,
		DiffBytes:  diffBytes,
		Extents:    extentCount,
	})
}

// cutoverRollback best-effort resumes the source VM after a failed
// cutover step, so a transient peer failure doesn't strand the guest
// paused forever, then writes the error response. vm.mu is already
// held by the caller.
func (s *Server) cutoverRollback(vm *VM, cause error, w http.ResponseWriter) {
	if err := vm.FC.Resume(); err != nil {
		s.logger.Error("cutover rollback resume failed", "vm", vm.ID, "err", err)
	} else {
		vm.State = api.StateRunning
	}
	writeError(w, http.StatusInternalServerError, cause)
}

// pushCutoverFiles sends the vmstate file and the diff mem extents
// to the peer concurrently over the pre-warmed peer client.
func (s *Server) pushCutoverFiles(ctx context.Context, extentsURL, stateURL, memPath, statePath string) (diffBytes int64, extentCount int, err error) {
	var wg sync.WaitGroup
	errs := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := s.postFile(ctx, stateURL, statePath); err != nil {
			errs <- fmt.Errorf("push vmstate: %w", err)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		db, ec, err := s.postExtents(ctx, extentsURL, memPath)
		if err != nil {
			errs <- fmt.Errorf("push mem extents: %w", err)
			return
		}
		diffBytes, extentCount = db, ec
	}()

	wg.Wait()
	close(errs)
	for e := range errs {
		if err == nil {
			err = e
		} else {
			err = fmt.Errorf("%w; %v", err, e)
		}
	}
	return diffBytes, extentCount, err
}
