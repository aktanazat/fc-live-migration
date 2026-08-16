package hostd

import (
	"fmt"
	"net/http"
	"path/filepath"
	"time"

	"github.com/aktanazat/fc-live-migration/internal/api"
	"github.com/aktanazat/fc-live-migration/internal/fc"
)

// The background checkpointer keeps every VM's base file close to its
// live memory: on each tick it takes a diff snapshot (which doSnapshot
// folds into the base), so the dirty set a migration must move never
// grows past roughly one interval of writes. Each tick freezes the
// guest for the diff write only — single-digit milliseconds at
// steady state, and one larger tick right after boot that absorbs
// kernel-boot dirt outside any migration window.
//
// Correctness note: a checkpointer diff taken between a migration's
// base sync and its cutover would fold pages into the local base that
// the target never receives (the dirty bitmap resets on every
// snapshot). The orchestrator therefore suspends the checkpointer for
// the duration of a migration via POST /vms/{id}/checkpointer.

// startCheckpointer launches the per-VM checkpoint loop. It stops
// when vm.ckptStop is closed (VM deletion or hostd shutdown). A nil
// or zero interval disables checkpointing entirely.
func (s *Server) startCheckpointer(vm *VM) {
	if s.ckptInterval <= 0 || vm.BaseMemPath == "" {
		return
	}
	vm.ckptStop = make(chan struct{})
	go func() {
		ticker := time.NewTicker(s.ckptInterval)
		defer ticker.Stop()
		dir := filepath.Join(vm.Dir, "ckpt")
		for {
			select {
			case <-vm.ckptStop:
				return
			case <-ticker.C:
			}
			vm.mu.Lock()
			if vm.State != api.StateRunning || vm.CkptSuspended {
				vm.mu.Unlock()
				continue
			}
			resp, _, err := s.doSnapshot(vm, fc.SnapshotDiff, dir, true)
			vm.mu.Unlock()
			if err != nil {
				s.logger.Error("background checkpoint failed", "vm", vm.ID, "err", err)
				continue
			}
			s.logger.Info("background checkpoint",
				"vm", vm.ID, "data_bytes", resp.DataBytes,
				"frozen_ms", resp.PauseMs+resp.SnapMs+resp.ResumeMs)
		}
	}()
}

// stopCheckpointer halts the VM's checkpoint loop. Safe to call for
// VMs that never had one. The caller must not hold vm.mu (the loop
// takes it).
func stopCheckpointer(vm *VM) {
	if vm.ckptStop != nil {
		close(vm.ckptStop)
		vm.ckptStop = nil
	}
}

// handleCheckpointer suspends or resumes the VM's background
// checkpointer. The orchestrator suspends it while a migration is in
// flight so no locally-merged diff can miss the target.
func (s *Server) handleCheckpointer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req api.CheckpointerRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	vm, ok := s.reg.get(id)
	if !ok {
		writeError(w, http.StatusNotFound, fmt.Errorf("vm %s not found", id))
		return
	}
	vm.mu.Lock()
	vm.CkptSuspended = !req.Enabled
	vm.mu.Unlock()
	writeJSON(w, http.StatusOK, api.CheckpointerResponse{Enabled: req.Enabled})
}
