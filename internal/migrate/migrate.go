// Package migrate drives a live Firecracker microVM migration between
// two hostd instances: it wraps hostd's REST contract in a typed
// client, then implements the pre-copy + cutover algorithm described
// in the project's migration contract.
package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aktanazat/fc-live-migration/internal/api"
)

// Client is a typed wrapper over one hostd instance's REST API.
type Client struct {
	baseURL string
	hc      *http.Client
}

// NewClient builds a Client for the hostd instance at baseURL, e.g.
// "http://127.0.0.1:8081". baseURL must not have a trailing slash
// requirement; one is stripped if present.
func NewClient(baseURL string) *Client {
	return &Client{baseURL: strings.TrimRight(baseURL, "/"), hc: &http.Client{}}
}

// httpJSON issues method against fullURL, marshaling body (if non-nil)
// as the JSON request payload and decoding a 2xx response into out
// (if non-nil). Non-2xx responses are turned into descriptive errors,
// preferring the api.Error envelope hostd sends.
func httpJSON(ctx context.Context, hc *http.Client, method, fullURL string, body, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s: %w", method, fullURL, err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, fullURL, reader)
	if err != nil {
		return fmt.Errorf("build request %s %s: %w", method, fullURL, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, fullURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		var apiErr api.Error
		if err := json.Unmarshal(data, &apiErr); err == nil && apiErr.Error != "" {
			return fmt.Errorf("%s %s: %s (status %d)", method, fullURL, apiErr.Error, resp.StatusCode)
		}
		return fmt.Errorf("%s %s: unexpected status %d: %s", method, fullURL, resp.StatusCode, string(data))
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			return fmt.Errorf("decode %s %s response: %w", method, fullURL, err)
		}
	}
	return nil
}

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	return httpJSON(ctx, c.hc, method, c.baseURL+path, body, out)
}

func vmPath(id, suffix string) string {
	p := "/vms/" + url.PathEscape(id)
	if suffix != "" {
		p += "/" + suffix
	}
	return p
}

// Healthz checks hostd's liveness endpoint.
func (c *Client) Healthz(ctx context.Context) error {
	var out struct {
		OK bool `json:"ok"`
	}
	if err := c.do(ctx, http.MethodGet, "/healthz", nil, &out); err != nil {
		return err
	}
	if !out.OK {
		return fmt.Errorf("healthz %s: not ok", c.baseURL)
	}
	return nil
}

// CreateVM boots a fresh microVM.
func (c *Client) CreateVM(ctx context.Context, req api.CreateVMRequest) (api.VMInfo, error) {
	var out api.VMInfo
	err := c.do(ctx, http.MethodPost, "/vms", req, &out)
	return out, err
}

// PrepareVM spawns a Firecracker process ready to receive a
// snapshot load, without booting a guest.
func (c *Client) PrepareVM(ctx context.Context, req api.PrepareRequest) (api.VMInfo, error) {
	var out api.VMInfo
	err := c.do(ctx, http.MethodPost, "/vms/prepare", req, &out)
	return out, err
}

// GetVM reports the current state of a microVM known to hostd.
func (c *Client) GetVM(ctx context.Context, id string) (api.VMInfo, error) {
	var out api.VMInfo
	err := c.do(ctx, http.MethodGet, vmPath(id, ""), nil, &out)
	return out, err
}

// DeleteVM kills Firecracker and removes hostd's state for id.
func (c *Client) DeleteVM(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, vmPath(id, ""), nil, nil)
}

// Pause pauses a running microVM.
func (c *Client) Pause(ctx context.Context, id string) (api.OpTiming, error) {
	var out api.OpTiming
	err := c.do(ctx, http.MethodPost, vmPath(id, "pause"), nil, &out)
	return out, err
}

// Resume resumes a paused microVM.
func (c *Client) Resume(ctx context.Context, id string) (api.OpTiming, error) {
	var out api.OpTiming
	err := c.do(ctx, http.MethodPost, vmPath(id, "resume"), nil, &out)
	return out, err
}

// Snapshot takes a full or diff snapshot of id.
func (c *Client) Snapshot(ctx context.Context, id string, req api.SnapshotRequest) (api.SnapshotResponse, error) {
	var out api.SnapshotResponse
	err := c.do(ctx, http.MethodPost, vmPath(id, "snapshot"), req, &out)
	return out, err
}

// Push streams snapshot files from this hostd to a peer hostd.
func (c *Client) Push(ctx context.Context, id string, req api.PushRequest) (api.PushResponse, error) {
	var out api.PushResponse
	err := c.do(ctx, http.MethodPost, vmPath(id, "push"), req, &out)
	return out, err
}

// Load loads a snapshot into a prepared VM.
func (c *Client) Load(ctx context.Context, id string, req api.LoadRequest) (api.OpTiming, error) {
	var out api.OpTiming
	err := c.do(ctx, http.MethodPost, vmPath(id, "load"), req, &out)
	return out, err
}

// Cutover executes the final migration phase on the source hostd:
// pause, final diff snapshot, push extents + vmstate, remote load.
func (c *Client) Cutover(ctx context.Context, id string, req api.CutoverRequest) (api.CutoverResponse, error) {
	var out api.CutoverResponse
	err := c.do(ctx, http.MethodPost, vmPath(id, "cutover"), req, &out)
	return out, err
}

// SetCheckpointer suspends (false) or resumes (true) the VM's
// background checkpointer on its host.
func (c *Client) SetCheckpointer(ctx context.Context, id string, enabled bool) error {
	var out api.CheckpointerResponse
	return c.do(ctx, http.MethodPost, vmPath(id, "checkpointer"), api.CheckpointerRequest{Enabled: enabled}, &out)
}

// Config parameterizes one end-to-end migration run.
type Config struct {
	VMID      string
	SourceURL string
	TargetURL string
	// PeerURL is the target hostd's base URL as reachable from the
	// source *container* (the source pushes snapshot bytes and issues
	// the cutover load host-to-host). The orchestrator-visible
	// TargetURL is usually a published localhost port and is not
	// dialable from inside the source container.
	PeerURL    string
	KernelPath string
	RootfsPath string
	KernelArgs string
	VCPUs      int64
	MemMiB     int64
	Tap        api.TapConfig

	// Settle, when non-nil, runs after the guest boots and before the
	// first pre-copy round. The CLI uses it to wait for the guest
	// workload to reach steady state (beacon packets flowing) so the
	// migration exercises a live guest, not one still booting, and to
	// reset the observer so its report covers only the migration.
	Settle func(context.Context) error

	// ThresholdBytes stops the pre-copy loop once a diff round's
	// DataBytes falls at or below this value. Zero selects the
	// default of 1 MiB.
	ThresholdBytes int64
	// MaxRounds caps the number of diff rounds attempted before
	// cutover runs regardless of convergence. Zero selects the
	// default of 8.
	MaxRounds int
}

const (
	// DefaultThresholdBytes is the default pre-copy convergence
	// threshold: once a diff round's dirtied data falls at or below
	// this many bytes, migrate proceeds straight to cutover.
	DefaultThresholdBytes int64 = 1 << 20 // 1 MiB
	// DefaultMaxRounds is the default cap on diff pre-copy rounds.
	DefaultMaxRounds = 8
)

func (c *Config) setDefaults() {
	if c.ThresholdBytes <= 0 {
		c.ThresholdBytes = DefaultThresholdBytes
	}
	if c.MaxRounds <= 0 {
		c.MaxRounds = DefaultMaxRounds
	}
}

func (c *Config) validate() error {
	switch {
	case c.VMID == "":
		return fmt.Errorf("config: vm id is required")
	case c.SourceURL == "":
		return fmt.Errorf("config: source url is required")
	case c.TargetURL == "":
		return fmt.Errorf("config: target url is required")
	case c.PeerURL == "":
		return fmt.Errorf("config: peer url is required")
	case c.KernelPath == "":
		return fmt.Errorf("config: kernel path is required")
	case c.RootfsPath == "":
		return fmt.Errorf("config: rootfs path is required")
	case c.VCPUs <= 0:
		return fmt.Errorf("config: vcpus must be positive, got %d", c.VCPUs)
	case c.MemMiB <= 0:
		return fmt.Errorf("config: mem_mib must be positive, got %d", c.MemMiB)
	case c.Tap.Name == "":
		return fmt.Errorf("config: tap name is required")
	case c.Tap.GuestMAC == "":
		return fmt.Errorf("config: tap guest MAC is required")
	}
	return nil
}

// RoundReport records the cost of one pre-copy diff round.
type RoundReport struct {
	Round     int     `json:"round"`
	DataBytes int64   `json:"data_bytes"`
	SnapMs    float64 `json:"snap_ms"`
	PushMs    float64 `json:"push_ms"`
}

// BaseSyncReport records the base checkpoint transfer, which happens
// while the guest keeps running and so contributes nothing to
// blackout.
type BaseSyncReport struct {
	Bytes int64   `json:"bytes"`
	Ms    float64 `json:"ms"`
}

// MigrationReport is the full timing record of one Orchestrate run.
// TotalBlackoutMs sums every window in which the guest was frozen
// during the migration: each diff round's pause+snapshot+resume plus
// the cutover blackout. The challenge bar (30ms) applies to this sum.
type MigrationReport struct {
	VMID            string              `json:"vm_id"`
	BaseSync        BaseSyncReport      `json:"base_sync"`
	Rounds          []RoundReport       `json:"rounds"`
	Cutover         api.CutoverResponse `json:"cutover"`
	TotalBlackoutMs float64             `json:"total_blackout_ms"`
	TotalMs         float64             `json:"total_ms"`
	StartedAt       time.Time           `json:"started_at"`
	FinishedAt      time.Time           `json:"finished_at"`
}

// snapshotWallMs is the window during which the guest is frozen for
// one snapshot: pause + the snapshot write + resume.
func snapshotWallMs(s api.SnapshotResponse) float64 {
	return s.PauseMs + s.SnapMs + s.ResumeMs
}

// Orchestrate runs the full live-migration algorithm: ensure the
// guest runs on the source (booting it if absent), pre-warm a
// prepared VM on the target, ship the source's base checkpoint while
// the guest keeps running, pre-copy shrinking diff rounds, cut over,
// then tear down the source VM.
func Orchestrate(ctx context.Context, cfg Config) (*MigrationReport, error) {
	cfg.setDefaults()
	if err := cfg.validate(); err != nil {
		return nil, err
	}

	src := NewClient(cfg.SourceURL)
	tgt := NewClient(cfg.TargetURL)

	started := time.Now()
	report := &MigrationReport{VMID: cfg.VMID, StartedAt: started}

	// Reuse a VM already running on the source (the reverse leg of a
	// previous migration, or one booted out of band); boot otherwise.
	info, err := src.GetVM(ctx, cfg.VMID)
	switch {
	case err == nil && info.State == api.StateRunning:
		// Migrate the existing guest.
	case err == nil:
		return nil, fmt.Errorf("vm %s on source %s is %s, not running", cfg.VMID, cfg.SourceURL, info.State)
	default:
		if info, err = src.CreateVM(ctx, api.CreateVMRequest{
			ID:              cfg.VMID,
			KernelPath:      cfg.KernelPath,
			RootfsPath:      cfg.RootfsPath,
			KernelArgs:      cfg.KernelArgs,
			VCPUs:           cfg.VCPUs,
			MemMiB:          cfg.MemMiB,
			Tap:             cfg.Tap,
			TrackDirtyPages: true,
		}); err != nil {
			return nil, fmt.Errorf("boot %s on source %s: %w", cfg.VMID, cfg.SourceURL, err)
		}
	}
	if info.BaseMemPath == "" || info.BaseStatePath == "" {
		return nil, fmt.Errorf("vm %s on source %s has no base checkpoint; it cannot be migrated", cfg.VMID, cfg.SourceURL)
	}

	if _, err := tgt.PrepareVM(ctx, api.PrepareRequest{ID: cfg.VMID, Tap: cfg.Tap}); err != nil {
		return nil, fmt.Errorf("prepare %s on target %s: %w", cfg.VMID, cfg.TargetURL, err)
	}

	if cfg.Settle != nil {
		if err := cfg.Settle(ctx); err != nil {
			return nil, fmt.Errorf("settle after boot: %w", err)
		}
	}

	// Suspend the source's background checkpointer for the duration
	// of the migration: a background diff taken after the base sync
	// would fold pages into the source's base that the target never
	// receives. Re-enabled on any failure; on success the source VM
	// is deleted anyway.
	if err := src.SetCheckpointer(ctx, cfg.VMID, false); err != nil {
		return nil, fmt.Errorf("suspend checkpointer: %w", err)
	}
	migrated := false
	defer func() {
		if !migrated {
			// Best-effort: the guest keeps running on the source, so
			// its checkpointer must keep running too.
			_ = src.SetCheckpointer(context.Background(), cfg.VMID, true)
		}
	}()

	srcBase := "/snapshots/" + cfg.VMID
	tgtBase := srcBase + "/base"

	// Checkpoint refresh (round 0): fold everything the guest has
	// dirtied since its standing base — for a young VM, the tail of
	// kernel boot — into the local base before shipping it. hostd
	// merges every diff snapshot into the base, so this is a plain
	// snapshot whose files are never pushed; the pages travel inside
	// the base sync instead of inflating the first pushed diff round.
	// Its pause is guest-frozen time and is counted in
	// TotalBlackoutMs like any other round.
	ckpt, err := src.Snapshot(ctx, cfg.VMID, api.SnapshotRequest{
		Type:   api.SnapshotDiff,
		Dir:    srcBase + "/ckpt",
		Resume: true,
	})
	if err != nil {
		return nil, fmt.Errorf("checkpoint refresh: %w", err)
	}
	report.Rounds = append(report.Rounds, RoundReport{
		Round:     0,
		DataBytes: ckpt.DataBytes,
		SnapMs:    snapshotWallMs(ckpt),
	})

	// Base sync: ship the source's standing base checkpoint (guest
	// memory as of its last snapshot or load) to the target. The
	// guest keeps running the whole time, so this contributes zero
	// blackout no matter how large the base is.
	baseSync, err := src.Push(ctx, cfg.VMID, api.PushRequest{TargetURL: cfg.PeerURL, RemoteDir: tgtBase,
		Files: []api.PushFile{
			{Path: info.BaseMemPath, Name: "mem", Sparse: true},
			{Path: info.BaseStatePath, Name: "vmstate", Sparse: false},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("base sync: %w", err)
	}
	report.BaseSync = BaseSyncReport{Bytes: baseSync.BytesSent, Ms: baseSync.Ms}

	// Diff rounds: keep pre-copying until a round's dirtied data
	// falls to the convergence threshold, capped at MaxRounds.
	//
	// Invariant: every diff snapshot taken MUST be pushed. Firecracker
	// resets the dirty-page bitmap on every snapshot, so an unpushed
	// diff's pages would appear in neither the base nor any later
	// diff — the target would silently restore stale memory. The
	// convergence check therefore runs only AFTER the push.
	for round := 1; round <= cfg.MaxRounds; round++ {
		diff, err := src.Snapshot(ctx, cfg.VMID, api.SnapshotRequest{
			Type:   api.SnapshotDiff,
			Dir:    fmt.Sprintf("%s/r%d", srcBase, round),
			Resume: true,
		})
		if err != nil {
			return nil, fmt.Errorf("round %d snapshot: %w", round, err)
		}
		diffPush, err := src.Push(ctx, cfg.VMID, api.PushRequest{TargetURL: cfg.PeerURL, RemoteDir: tgtBase,
			Files: []api.PushFile{
				{Path: diff.MemPath, Name: "mem", Sparse: true},
			},
		})
		if err != nil {
			return nil, fmt.Errorf("round %d push: %w", round, err)
		}
		report.Rounds = append(report.Rounds, RoundReport{
			Round:     round,
			DataBytes: diff.DataBytes,
			SnapMs:    snapshotWallMs(diff),
			PushMs:    diffPush.Ms,
		})
		if diff.DataBytes <= cfg.ThresholdBytes {
			break
		}
	}

	cutover, err := src.Cutover(ctx, cfg.VMID, api.CutoverRequest{TargetURL: cfg.PeerURL, TargetID: cfg.VMID,
		RemoteDir:     tgtBase,
		RemoteMemName: "mem",
		LocalDir:      srcBase + "/cutover"})
	if err != nil {
		return nil, fmt.Errorf("cutover: %w", err)
	}
	report.Cutover = cutover
	migrated = true

	if err := src.DeleteVM(ctx, cfg.VMID); err != nil {
		return nil, fmt.Errorf("delete source vm %s: %w", cfg.VMID, err)
	}

	for _, rr := range report.Rounds {
		report.TotalBlackoutMs += rr.SnapMs
	}
	report.TotalBlackoutMs += cutover.BlackoutMs
	finished := time.Now()
	report.FinishedAt = finished
	report.TotalMs = float64(finished.Sub(started).Microseconds()) / 1000.0
	return report, nil
}
