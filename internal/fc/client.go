// Package fc is a thin client over the Firecracker v1.14.4 HTTP API,
// exposed by each microVM process on a private unix socket. One
// Client is spawned per VM and lives for the VM's whole lifetime:
// boot, pause/resume across snapshot rounds, and eventual kill.
package fc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// SnapshotType is the Firecracker wire value for a snapshot flavor
// ("Full" or "Diff" — note the capitalization differs from the
// hostd/orchestrator contract's lowercase api.SnapshotType).
type SnapshotType string

const (
	SnapshotFull SnapshotType = "Full"
	SnapshotDiff SnapshotType = "Diff"
)

// BootConfig configures and boots a freshly spawned Firecracker
// process into a running guest.
type BootConfig struct {
	KernelPath      string
	RootfsPath      string
	KernelArgs      string
	VCPUs           int64
	MemMiB          int64
	TapName         string
	GuestMAC        string
	TrackDirtyPages bool
}

// Client drives one Firecracker process's API socket.
type Client struct {
	id       string
	sockPath string
	logPath  string

	http *http.Client

	mu     sync.Mutex
	cmd    *exec.Cmd
	killed bool
}

// SpawnProcess execs the Firecracker binary at binPath with a fresh
// API socket at sockPath and structured logging to logPath, and
// waits for the socket to start accepting connections. The returned
// Client is unconfigured: call ConfigureAndBoot to start a guest, or
// LoadSnapshot to restore one.
func SpawnProcess(binPath, id, sockPath, logPath string) (*Client, error) {
	if err := os.RemoveAll(sockPath); err != nil {
		return nil, fmt.Errorf("remove stale socket %s: %w", sockPath, err)
	}

	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, fmt.Errorf("create firecracker log file %s: %w", logPath, err)
	}
	defer logFile.Close()

	cmd := exec.Command(binPath, "--api-sock", sockPath, "--log-path", logPath, "--level", "Error")
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start firecracker for vm %s: %w", id, err)
	}

	c := &Client{
		id:       id,
		sockPath: sockPath,
		logPath:  logPath,
		cmd:      cmd,
		http: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", sockPath)
				},
			},
		},
	}

	if err := waitForSocket(sockPath, 2*time.Second); err != nil {
		_ = c.Kill()
		return nil, fmt.Errorf("vm %s: %w", id, err)
	}
	return c, nil
}

// waitForSocket polls path until a unix connection succeeds or
// timeout elapses.
func waitForSocket(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.Dial("unix", path)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
		time.Sleep(time.Millisecond)
	}
	return fmt.Errorf("socket %s not ready after %s: %w", path, timeout, lastErr)
}

// PID returns the Firecracker process id.
func (c *Client) PID() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cmd == nil || c.cmd.Process == nil {
		return 0
	}
	return c.cmd.Process.Pid
}

// ConfigureAndBoot wires machine config, boot source, root drive,
// and network interface into a freshly spawned process, then starts
// the guest.
func (c *Client) ConfigureAndBoot(cfg BootConfig) error {
	type machineConfig struct {
		VCPUCount       int64 `json:"vcpu_count"`
		MemSizeMiB      int64 `json:"mem_size_mib"`
		TrackDirtyPages bool  `json:"track_dirty_pages"`
	}
	type bootSource struct {
		KernelImagePath string `json:"kernel_image_path"`
		BootArgs        string `json:"boot_args,omitempty"`
	}
	type drive struct {
		DriveID      string `json:"drive_id"`
		PathOnHost   string `json:"path_on_host"`
		IsRootDevice bool   `json:"is_root_device"`
		IsReadOnly   bool   `json:"is_read_only"`
	}
	type networkInterface struct {
		IfaceID     string `json:"iface_id"`
		GuestMAC    string `json:"guest_mac"`
		HostDevName string `json:"host_dev_name"`
	}
	type action struct {
		ActionType string `json:"action_type"`
	}

	if err := c.put("/machine-config", machineConfig{
		VCPUCount:       cfg.VCPUs,
		MemSizeMiB:      cfg.MemMiB,
		TrackDirtyPages: cfg.TrackDirtyPages,
	}); err != nil {
		return fmt.Errorf("machine-config: %w", err)
	}
	if err := c.put("/boot-source", bootSource{
		KernelImagePath: cfg.KernelPath,
		BootArgs:        cfg.KernelArgs,
	}); err != nil {
		return fmt.Errorf("boot-source: %w", err)
	}
	if err := c.put("/drives/rootfs", drive{
		DriveID:      "rootfs",
		PathOnHost:   cfg.RootfsPath,
		IsRootDevice: true,
		// The rootfs is shared storage: both hosts see the identical
		// file at the identical path, so the drive must be read-only
		// (the live-migration model migrates RAM, not disks).
		IsReadOnly: true,
	}); err != nil {
		return fmt.Errorf("drives/rootfs: %w", err)
	}
	if err := c.put("/network-interfaces/eth0", networkInterface{
		IfaceID:     "eth0",
		GuestMAC:    cfg.GuestMAC,
		HostDevName: cfg.TapName,
	}); err != nil {
		return fmt.Errorf("network-interfaces/eth0: %w", err)
	}
	if err := c.put("/actions", action{ActionType: "InstanceStart"}); err != nil {
		return fmt.Errorf("instance-start: %w", err)
	}
	return nil
}

// Pause freezes the guest's vCPUs (PATCH /vm {state: Paused}).
func (c *Client) Pause() error {
	if err := c.patchVMState("Paused"); err != nil {
		return fmt.Errorf("pause: %w", err)
	}
	return nil
}

// Resume unfreezes the guest's vCPUs (PATCH /vm {state: Resumed}).
func (c *Client) Resume() error {
	if err := c.patchVMState("Resumed"); err != nil {
		return fmt.Errorf("resume: %w", err)
	}
	return nil
}

func (c *Client) patchVMState(state string) error {
	type vmState struct {
		State string `json:"state"`
	}
	return c.do(http.MethodPatch, "/vm", vmState{State: state})
}

// CreateSnapshot pauses-guest-required snapshotting: the VM must
// already be paused. statePath and memPath name the vmstate and
// memory files Firecracker writes.
func (c *Client) CreateSnapshot(snapType SnapshotType, statePath, memPath string) error {
	type snapshotCreateRequest struct {
		SnapshotType string `json:"snapshot_type"`
		SnapshotPath string `json:"snapshot_path"`
		MemFilePath  string `json:"mem_file_path"`
	}
	if err := c.put("/snapshot/create", snapshotCreateRequest{
		SnapshotType: string(snapType),
		SnapshotPath: statePath,
		MemFilePath:  memPath,
	}); err != nil {
		return fmt.Errorf("snapshot/create: %w", err)
	}
	return nil
}

// LoadSnapshot restores a snapshot into this Client's Firecracker
// process, which must be freshly spawned and unconfigured. When
// resumeVM is true the guest resumes execution as part of this call.
func (c *Client) LoadSnapshot(statePath, memPath string, trackDirtyPages, resumeVM bool) error {
	type memBackend struct {
		BackendType string `json:"backend_type"`
		BackendPath string `json:"backend_path"`
	}
	type snapshotLoadRequest struct {
		SnapshotPath    string     `json:"snapshot_path"`
		MemBackend      memBackend `json:"mem_backend"`
		TrackDirtyPages bool       `json:"track_dirty_pages"`
		ResumeVM        bool       `json:"resume_vm"`
	}
	if err := c.put("/snapshot/load", snapshotLoadRequest{
		SnapshotPath:    statePath,
		MemBackend:      memBackend{BackendType: "File", BackendPath: memPath},
		TrackDirtyPages: trackDirtyPages,
		ResumeVM:        resumeVM,
	}); err != nil {
		return fmt.Errorf("snapshot/load: %w", err)
	}
	return nil
}

// Kill sends SIGKILL to the Firecracker process, reaps it, and
// removes its API socket. It is safe to call more than once.
func (c *Client) Kill() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.killed {
		return nil
	}
	c.killed = true

	if c.cmd != nil && c.cmd.Process != nil {
		if err := c.cmd.Process.Signal(syscall.SIGKILL); err != nil && !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("kill vm %s (pid %d): %w", c.id, c.cmd.Process.Pid, err)
		}
		if err := c.cmd.Wait(); err != nil {
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				return fmt.Errorf("reap vm %s (pid %d): %w", c.id, c.cmd.Process.Pid, err)
			}
		}
	}
	if err := os.RemoveAll(c.sockPath); err != nil {
		return fmt.Errorf("remove socket %s: %w", c.sockPath, err)
	}
	return nil
}

// do performs a Firecracker API call and discards the (small) JSON
// response body, treating any non-2xx status as an error.
func (c *Client) do(method, path string, body any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, "http://unix"+path, reader)
	if err != nil {
		return fmt.Errorf("new request %s %s: %w", method, path, err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: status %d: %s", method, path, resp.StatusCode, string(data))
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

func (c *Client) put(path string, body any) error {
	return c.do(http.MethodPut, path, body)
}
