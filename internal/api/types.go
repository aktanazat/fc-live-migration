// Package api defines the REST contract between the migration
// orchestrator (migratectl) and the per-host agent (hostd).
//
// Endpoints served by hostd (:8080):
//
//	GET    /healthz                    → 200 {"ok":true}
//	POST   /vms                        CreateVMRequest  → 201 VMInfo   (boot a fresh microVM)
//	POST   /vms/prepare                PrepareRequest   → 201 VMInfo   (spawn Firecracker + tap, no boot; awaits /load)
//	GET    /vms/{id}                   → VMInfo
//	DELETE /vms/{id}                   → 204            (kill Firecracker, remove state)
//	POST   /vms/{id}/pause             → OpTiming
//	POST   /vms/{id}/resume            → OpTiming
//	POST   /vms/{id}/snapshot          SnapshotRequest  → SnapshotResponse
//	POST   /vms/{id}/push              PushRequest      → PushResponse (stream files to a peer hostd)
//	POST   /vms/{id}/load              LoadRequest      → OpTiming     (snapshot/load into a prepared VM)
//	POST   /vms/{id}/cutover           CutoverRequest   → CutoverResponse (source-side final phase)
//	POST   /vms/{id}/checkpointer      CheckpointerRequest → CheckpointerResponse (suspend/resume background checkpointing)
//	POST   /files/base?dir=&name=      octet-stream     → FileWriteResponse (receive a whole file)
//	POST   /files/extents?dir=&name=&size=  extent stream → FileWriteResponse (apply sparse extents in place)
//
// Extent wire format (POST /files/extents body): repeated frames of
// [offset uint64 LE][length uint64 LE][length bytes of data] until EOF.
// The receiver pwrites each frame at its offset into the named file,
// then truncates it to `size` (the sender's apparent file size) so
// trailing holes survive the transfer and the result is a
// byte-identical sparse replica.
package api

// VMState is the lifecycle state of a microVM as reported by hostd.
type VMState string

const (
	// StatePrepared means a Firecracker process exists with its API
	// socket up, but no guest has been booted or loaded yet.
	StatePrepared VMState = "prepared"
	StateRunning  VMState = "running"
	StatePaused   VMState = "paused"
	StateStopped  VMState = "stopped"
)

// SnapshotType selects a Firecracker snapshot flavor.
type SnapshotType string

const (
	SnapshotFull SnapshotType = "full"
	SnapshotDiff SnapshotType = "diff"
)

// TapConfig describes the tap device wired to the guest NIC. The tap
// is created by the container entrypoint and bridged (br0) with the
// container's eth0, so the guest owns a first-class IP on the Docker
// network and keeps it across migration.
type TapConfig struct {
	Name     string `json:"name"`      // e.g. "tap0"
	GuestMAC string `json:"guest_mac"` // e.g. "AA:FC:00:00:00:01"
}

// CreateVMRequest boots a new microVM from a kernel and rootfs.
type CreateVMRequest struct {
	ID              string    `json:"id"`
	KernelPath      string    `json:"kernel_path"`
	RootfsPath      string    `json:"rootfs_path"`
	KernelArgs      string    `json:"kernel_args,omitempty"`
	VCPUs           int64     `json:"vcpus"`
	MemMiB          int64     `json:"mem_mib"`
	Tap             TapConfig `json:"tap"`
	TrackDirtyPages bool      `json:"track_dirty_pages"`
}

// PrepareRequest spawns a Firecracker process ready to receive a
// snapshot load. Pre-warming this out of the critical path keeps
// process startup cost out of the migration blackout window.
type PrepareRequest struct {
	ID  string    `json:"id"`
	Tap TapConfig `json:"tap"`
}

// VMInfo reports a microVM known to hostd.
//
// Base checkpoint invariant: for every VM hostd can migrate,
// BaseMemPath names a local file equal to guest memory as of the last
// snapshot or load, and the dirty-page bitmap tracks writes since
// then. A freshly booted VM gets a full snapshot at provisioning
// time; a restored VM's base is the memory file it was loaded from;
// every subsequent diff snapshot is merged into the base. Migration
// therefore ships the base while the guest keeps running, and only
// diff rounds and the cutover pause the guest.
type VMInfo struct {
	ID            string  `json:"id"`
	State         VMState `json:"state"`
	PID           int     `json:"pid,omitempty"`
	BaseMemPath   string  `json:"base_mem_path,omitempty"`
	BaseStatePath string  `json:"base_state_path,omitempty"`
}

// OpTiming reports how long a single operation took.
type OpTiming struct {
	Ms float64 `json:"ms"`
}

// SnapshotRequest creates a snapshot of a paused or running VM.
// hostd pauses the VM if needed; Resume controls whether the VM is
// resumed after the snapshot is written.
type SnapshotRequest struct {
	Type   SnapshotType `json:"type"`
	Dir    string       `json:"dir"` // e.g. "/snapshots/vm0/r3"
	Resume bool         `json:"resume"`
}

// SnapshotResponse reports the artifacts of a snapshot.
type SnapshotResponse struct {
	StatePath string  `json:"state_path"`
	MemPath   string  `json:"mem_path"`
	MemBytes  int64   `json:"mem_bytes"`  // apparent file size
	DataBytes int64   `json:"data_bytes"` // allocated (non-hole) bytes
	PauseMs   float64 `json:"pause_ms"`
	SnapMs    float64 `json:"snap_ms"`
	ResumeMs  float64 `json:"resume_ms"`
}

// PushRequest streams snapshot files from this hostd to a peer hostd.
// Files with Sparse=true are sent as data extents (see package doc);
// others are sent whole.
type PushRequest struct {
	TargetURL string     `json:"target_url"` // e.g. "http://host-b:8080"
	RemoteDir string     `json:"remote_dir"` // destination dir on the peer
	Files     []PushFile `json:"files"`
}

// PushFile names one file to push.
type PushFile struct {
	Path   string `json:"path"`   // local path on the source
	Name   string `json:"name"`   // file name at the destination
	Sparse bool   `json:"sparse"` // send only allocated extents
}

// PushResponse reports transfer volume and duration.
type PushResponse struct {
	BytesSent int64   `json:"bytes_sent"`
	Extents   int     `json:"extents"`
	Ms        float64 `json:"ms"`
}

// LoadRequest loads a snapshot into a prepared VM.
type LoadRequest struct {
	StatePath       string `json:"state_path"`
	MemPath         string `json:"mem_path"`
	TrackDirtyPages bool   `json:"track_dirty_pages"`
	Resume          bool   `json:"resume"` // resume in the same Firecracker call
}

// CutoverRequest executes the final migration phase entirely on the
// source hostd, host-to-host, with no orchestrator round trips inside
// the blackout window: pause → final diff snapshot → push extents +
// vmstate → remote load(resume=true).
type CutoverRequest struct {
	TargetURL string `json:"target_url"`
	TargetID  string `json:"target_id"`  // prepared VM id on the target
	RemoteDir string `json:"remote_dir"` // peer dir holding the base mem file
	// RemoteMemName is the file under RemoteDir that diff extents are
	// applied onto; it must already equal base+diffs from prior rounds.
	RemoteMemName string `json:"remote_mem_name"`
	LocalDir      string `json:"local_dir"` // where the final diff is written
}

// CutoverResponse breaks the blackout window into its phases.
// BlackoutMs = pause start → remote resume acknowledged.
type CutoverResponse struct {
	PauseMs    float64 `json:"pause_ms"`
	SnapMs     float64 `json:"snap_ms"`
	PushMs     float64 `json:"push_ms"`
	LoadMs     float64 `json:"load_ms"`
	BlackoutMs float64 `json:"blackout_ms"`
	DiffBytes  int64   `json:"diff_bytes"`
	Extents    int     `json:"extents"`
}

// FileWriteResponse acknowledges a received file or extent stream.
type FileWriteResponse struct {
	BytesWritten int64 `json:"bytes_written"`
	Extents      int   `json:"extents"`
}

// Error is the JSON body of every non-2xx hostd response.
type Error struct {
	Error string `json:"error"`
}

// CheckpointerRequest suspends (Enabled=false) or resumes
// (Enabled=true) a VM's background checkpointer. A migration must
// suspend it: a background diff taken after the base sync would fold
// pages into the source's base that the target never receives.
type CheckpointerRequest struct {
	Enabled bool `json:"enabled"`
}

// CheckpointerResponse echoes the resulting checkpointer state.
type CheckpointerResponse struct {
	Enabled bool `json:"enabled"`
}
