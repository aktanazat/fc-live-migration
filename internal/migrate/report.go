package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// BlackoutThresholdMs is the acceptance bar for a migration's
// blackout window: both the source-reported CutoverResponse.BlackoutMs
// and the observer's max_gap_ms must fall at or under this to pass.
const BlackoutThresholdMs = 30.0

// Passes reports whether a measured gap, in milliseconds, meets the
// blackout acceptance bar.
func Passes(ms float64) bool {
	return ms <= BlackoutThresholdMs
}

// WriteHuman renders the report as aligned plain text: a header, one
// line per pre-copy round, and a cutover phase breakdown.
func (r *MigrationReport) WriteHuman(w io.Writer) {
	fmt.Fprintf(w, "migration report: vm=%s started=%s finished=%s total_ms=%.3f\n",
		r.VMID, r.StartedAt.Format(time.RFC3339), r.FinishedAt.Format(time.RFC3339), r.TotalMs)

	fmt.Fprintln(w, "rounds:")
	for _, rr := range r.Rounds {
		kind := "diff"
		if rr.Round == 0 {
			kind = "full"
		}
		fmt.Fprintf(w, "  round %-2d %-4s  data=%12d B  snap=%9.3fms  push=%9.3fms\n",
			rr.Round, kind, rr.DataBytes, rr.SnapMs, rr.PushMs)
	}

	fmt.Fprintln(w, "cutover:")
	fmt.Fprintf(w, "  %-10s %10s\n", "phase", "ms")
	fmt.Fprintf(w, "  %-10s %10.3f\n", "pause", r.Cutover.PauseMs)
	fmt.Fprintf(w, "  %-10s %10.3f\n", "snapshot", r.Cutover.SnapMs)
	fmt.Fprintf(w, "  %-10s %10.3f\n", "push", r.Cutover.PushMs)
	fmt.Fprintf(w, "  %-10s %10.3f\n", "load", r.Cutover.LoadMs)
	fmt.Fprintf(w, "  %-10s %10.3f\n", "blackout", r.Cutover.BlackoutMs)
	fmt.Fprintf(w, "  diff_bytes=%d extents=%d\n", r.Cutover.DiffBytes, r.Cutover.Extents)
}

// WriteJSON renders the report as indented JSON.
func (r *MigrationReport) WriteJSON(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(r)
}

// GapEntry is one inter-arrival gap recorded by the observer.
type GapEntry struct {
	Seq   uint64    `json:"seq"`
	GapMs float64   `json:"gap_ms"`
	At    time.Time `json:"at"`
}

// ObserverReport is the observer's /report payload: beacon packet
// arrival statistics over the current measurement window.
type ObserverReport struct {
	Packets   int64      `json:"packets"`
	MaxGapMs  float64    `json:"max_gap_ms"`
	Gaps      []GapEntry `json:"gaps"`
	LastSeq   uint64     `json:"last_seq"`
	StartedAt time.Time  `json:"started_at"`
}

// ObserverClient is a typed wrapper over the observer's small HTTP
// API used to bracket a migration run and read back the blackout it
// measured on the guest's beacon traffic.
type ObserverClient struct {
	baseURL string
	hc      *http.Client
}

// NewObserverClient builds an ObserverClient for the observer at
// baseURL, e.g. "http://127.0.0.1:9090".
func NewObserverClient(baseURL string) *ObserverClient {
	return &ObserverClient{baseURL: strings.TrimRight(baseURL, "/"), hc: &http.Client{}}
}

// Reset clears the observer's accumulated statistics so a subsequent
// Report reflects only what happens after this call.
func (c *ObserverClient) Reset(ctx context.Context) error {
	return httpJSON(ctx, c.hc, http.MethodPost, c.baseURL+"/reset", nil, nil)
}

// Report fetches the observer's current beacon arrival statistics.
func (c *ObserverClient) Report(ctx context.Context) (ObserverReport, error) {
	var out ObserverReport
	err := httpJSON(ctx, c.hc, http.MethodGet, c.baseURL+"/report", nil, &out)
	return out, err
}
