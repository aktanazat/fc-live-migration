package hostd

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/aktanazat/fc-live-migration/internal/api"
	"github.com/aktanazat/fc-live-migration/internal/fc"
	"github.com/aktanazat/fc-live-migration/internal/sparse"
)

// handleCreateVM boots a fresh microVM from a kernel and rootfs:
// spawn the Firecracker process, configure it, and start the guest.
func (s *Server) handleCreateVM(w http.ResponseWriter, r *http.Request) {
	var req api.CreateVMRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := validateCreateVMRequest(req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}

	dir := s.vmDir(req.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("mkdir %s: %w", dir, err))
		return
	}

	client, err := fc.SpawnProcess(s.fcBin, req.ID, s.sockPath(req.ID), s.logPath(req.ID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("spawn firecracker: %w", err))
		return
	}
	if err := client.ConfigureAndBoot(fc.BootConfig{
		KernelPath:      req.KernelPath,
		RootfsPath:      req.RootfsPath,
		KernelArgs:      req.KernelArgs,
		VCPUs:           req.VCPUs,
		MemMiB:          req.MemMiB,
		TapName:         req.Tap.Name,
		GuestMAC:        req.Tap.GuestMAC,
		TrackDirtyPages: req.TrackDirtyPages,
	}); err != nil {
		_ = client.Kill()
		writeError(w, http.StatusInternalServerError, fmt.Errorf("configure and boot: %w", err))
		return
	}

	vm := &VM{
		ID:              req.ID,
		State:           api.StateRunning,
		Tap:             req.Tap,
		TrackDirtyPages: req.TrackDirtyPages,
		Dir:             dir,
		SockPath:        s.sockPath(req.ID),
		LogPath:         s.logPath(req.ID),
		FC:              client,
	}
	if !s.reg.add(vm) {
		_ = client.Kill()
		writeError(w, http.StatusConflict, fmt.Errorf("vm %s already exists", req.ID))
		return
	}

	// Establish the base checkpoint invariant (see api.VMInfo): a
	// full snapshot taken at provisioning time, before any workload
	// exists, so no later migration ever needs a pause proportional
	// to full guest memory. The snapshot resets the dirty-page
	// bitmap, so the bitmap tracks exactly the writes after this
	// base.
	if req.TrackDirtyPages {
		baseDir := filepath.Join(dir, "base")
		if err := s.takeBaseSnapshot(vm, baseDir); err != nil {
			_ = client.Kill()
			s.reg.remove(req.ID)
			writeError(w, http.StatusInternalServerError, fmt.Errorf("provisioning base snapshot: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusCreated, vm.Info())
}

// takeBaseSnapshot pauses vm, writes a full snapshot into baseDir,
// resumes, and records the base paths. Called at provisioning time
// while the caller still owns the VM exclusively.
func (s *Server) takeBaseSnapshot(vm *VM, baseDir string) error {
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", baseDir, err)
	}
	memPath := filepath.Join(baseDir, snapshotMemName)
	statePath := filepath.Join(baseDir, snapshotStateName)
	if err := vm.FC.Pause(); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	if err := vm.FC.CreateSnapshot(fc.SnapshotFull, statePath, memPath); err != nil {
		return fmt.Errorf("create full snapshot: %w", err)
	}
	if err := vm.FC.Resume(); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	vm.BaseMemPath = memPath
	vm.BaseStatePath = statePath
	return nil
}

func validateCreateVMRequest(req api.CreateVMRequest) error {
	switch {
	case req.ID == "":
		return fmt.Errorf("id is required")
	case req.KernelPath == "":
		return fmt.Errorf("kernel_path is required")
	case req.RootfsPath == "":
		return fmt.Errorf("rootfs_path is required")
	case req.VCPUs <= 0:
		return fmt.Errorf("vcpus must be positive")
	case req.MemMiB <= 0:
		return fmt.Errorf("mem_mib must be positive")
	case req.Tap.Name == "":
		return fmt.Errorf("tap.name is required")
	case req.Tap.GuestMAC == "":
		return fmt.Errorf("tap.guest_mac is required")
	}
	return nil
}

// handlePrepareVM spawns a bare, unconfigured Firecracker process
// that awaits a future /load, keeping process-startup cost out of
// the migration blackout window.
func (s *Server) handlePrepareVM(w http.ResponseWriter, r *http.Request) {
	var req api.PrepareRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.ID == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("id is required"))
		return
	}

	dir := s.vmDir(req.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("mkdir %s: %w", dir, err))
		return
	}

	client, err := fc.SpawnProcess(s.fcBin, req.ID, s.sockPath(req.ID), s.logPath(req.ID))
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("spawn firecracker: %w", err))
		return
	}

	vm := &VM{
		ID:       req.ID,
		State:    api.StatePrepared,
		Tap:      req.Tap,
		Dir:      dir,
		SockPath: s.sockPath(req.ID),
		LogPath:  s.logPath(req.ID),
		FC:       client,
	}
	if !s.reg.add(vm) {
		_ = client.Kill()
		writeError(w, http.StatusConflict, fmt.Errorf("vm %s already exists", req.ID))
		return
	}
	writeJSON(w, http.StatusCreated, vm.Info())
}

func (s *Server) handleGetVM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vm, ok := s.reg.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("vm %s not found", id))
		return
	}
	vm.mu.Lock()
	info := vm.Info()
	vm.mu.Unlock()
	writeJSON(w, http.StatusOK, info)
}

// handleDeleteVM kills the Firecracker process and removes the VM's
// on-disk state.
func (s *Server) handleDeleteVM(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vm, ok := s.reg.remove(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("vm %s not found", id))
		return
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if err := vm.FC.Kill(); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("kill vm %s: %w", id, err))
		return
	}
	if err := os.RemoveAll(vm.Dir); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("remove %s: %w", vm.Dir, err))
		return
	}
	if err := os.RemoveAll(vm.LogPath); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("remove %s: %w", vm.LogPath, err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
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
	start := time.Now()
	if err := vm.FC.Pause(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ms := msSince(start)
	vm.State = api.StatePaused
	writeJSON(w, http.StatusOK, api.OpTiming{Ms: ms})
}

func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	vm, ok := s.reg.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("vm %s not found", id))
		return
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.State != api.StatePaused {
		writeError(w, http.StatusConflict, fmt.Errorf("vm %s is %s, not paused", id, vm.State))
		return
	}
	start := time.Now()
	if err := vm.FC.Resume(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	ms := msSince(start)
	vm.State = api.StateRunning
	writeJSON(w, http.StatusOK, api.OpTiming{Ms: ms})
}

// snapshotFileNames are fixed so every producer and consumer of a
// snapshot round (push, cutover, remote load) agrees on them without
// extra negotiation.
const (
	snapshotStateName = "vmstate"
	snapshotMemName   = "mem"
)

// handleSnapshot creates a full or diff snapshot of a VM, pausing it
// first if it is currently running.
func (s *Server) handleSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.SnapshotRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	snapType, err := toFCSnapshotType(req.Type)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Dir == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("dir is required"))
		return
	}

	vm, ok := s.reg.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("vm %s not found", id))
		return
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()

	var pauseMs float64
	switch vm.State {
	case api.StateRunning:
		pauseStart := time.Now()
		if err := vm.FC.Pause(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("pause: %w", err))
			return
		}
		pauseMs = msSince(pauseStart)
		vm.State = api.StatePaused
	case api.StatePaused:
		// Already paused; nothing to do.
	default:
		writeError(w, http.StatusConflict, fmt.Errorf("vm %s is %s, cannot snapshot", id, vm.State))
		return
	}

	if err := os.MkdirAll(req.Dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("mkdir %s: %w", req.Dir, err))
		return
	}
	statePath := filepath.Join(req.Dir, snapshotStateName)
	memPath := filepath.Join(req.Dir, snapshotMemName)

	snapStart := time.Now()
	if err := vm.FC.CreateSnapshot(snapType, statePath, memPath); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("create snapshot: %w", err))
		return
	}
	snapMs := msSince(snapStart)

	var resumeMs float64
	if req.Resume {
		resumeStart := time.Now()
		if err := vm.FC.Resume(); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("resume: %w", err))
			return
		}
		resumeMs = msSince(resumeStart)
		vm.State = api.StateRunning
	}

	// Maintain the base checkpoint invariant: every diff snapshot's
	// dirty pages fold into the local base, keeping BaseMemPath equal
	// to guest memory as of this snapshot. Runs after the resume so
	// the merge cost never extends the guest's frozen window. The
	// base dir itself is exempt (the provisioning snapshot writes
	// there directly).
	if snapType == fc.SnapshotDiff && vm.BaseMemPath != "" && memPath != vm.BaseMemPath {
		if _, _, err := sparse.Merge(vm.BaseMemPath, memPath); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("merge diff into base: %w", err))
			return
		}
		if err := copyFile(vm.BaseStatePath, statePath); err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("refresh base vmstate: %w", err))
			return
		}
	}

	memInfo, err := os.Stat(memPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("stat %s: %w", memPath, err))
		return
	}
	dataBytes, err := sparse.AllocatedBytes(memPath)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	writeJSON(w, http.StatusOK, api.SnapshotResponse{
		StatePath: statePath,
		MemPath:   memPath,
		MemBytes:  memInfo.Size(),
		DataBytes: dataBytes,
		PauseMs:   pauseMs,
		SnapMs:    snapMs,
		ResumeMs:  resumeMs,
	})
}

func toFCSnapshotType(t api.SnapshotType) (fc.SnapshotType, error) {
	switch t {
	case api.SnapshotFull:
		return fc.SnapshotFull, nil
	case api.SnapshotDiff:
		return fc.SnapshotDiff, nil
	default:
		return "", fmt.Errorf("unknown snapshot type %q", t)
	}
}

// handleLoad restores a snapshot into a prepared VM.
func (s *Server) handleLoad(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.LoadRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.StatePath == "" || req.MemPath == "" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("state_path and mem_path are required"))
		return
	}

	vm, ok := s.reg.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("vm %s not found", id))
		return
	}
	vm.mu.Lock()
	defer vm.mu.Unlock()
	if vm.State != api.StatePrepared {
		writeError(w, http.StatusConflict, fmt.Errorf("vm %s is %s, not prepared", id, vm.State))
		return
	}

	start := time.Now()
	if err := vm.FC.LoadSnapshot(req.StatePath, req.MemPath, req.TrackDirtyPages, req.Resume); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("load snapshot: %w", err))
		return
	}
	ms := msSince(start)

	vm.TrackDirtyPages = req.TrackDirtyPages
	vm.BaseMemPath = req.MemPath
	vm.BaseStatePath = req.StatePath
	if req.Resume {
		vm.State = api.StateRunning
	} else {
		vm.State = api.StatePaused
	}
	writeJSON(w, http.StatusOK, api.OpTiming{Ms: ms})
}
