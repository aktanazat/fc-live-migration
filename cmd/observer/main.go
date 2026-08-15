// Command observer listens for beacon UDP heartbeats on :9999 and
// tracks inter-arrival gaps using its own monotonic clock. Results are
// served over HTTP on :9090.
//
// Gap timing is computed entirely from the observer's local receipt
// time, not the guest's embedded timestamp: during a live migration
// the guest's own clock is paused along with the VM, so only an
// independent clock can measure the true wall-clock blackout window.
package main

import (
	"encoding/binary"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"
)

const (
	udpAddr  = ":9999"
	httpAddr = ":9090"

	// beaconPacketSize is the beacon wire format:
	// [seq uint64 LE][guest monotonic ns uint64 LE].
	beaconPacketSize = 16

	// topGapsLimit bounds how many of the largest gaps are retained.
	topGapsLimit = 10

	// logGapThresholdMs is the gap size above which an arrival is
	// logged to stdout.
	logGapThresholdMs = 10.0
)

// gap records one inter-arrival gap.
type gap struct {
	seq      uint64
	gapMs    float64
	atUnixMs int64
}

// stats accumulates beacon arrival statistics under a mutex; it is
// shared between the UDP receive loop and the HTTP handlers.
type stats struct {
	mu sync.Mutex

	packets     uint64
	haveLast    bool
	lastArrival time.Time
	lastSeq     uint64
	maxGapMs    float64
	topGaps     []gap
	since       time.Time
}

func newStats() *stats {
	return &stats{since: time.Now()}
}

// record accounts for one beacon packet with the given sequence
// number, observed at arrival (the observer's own monotonic clock).
func (s *stats) record(seq uint64, arrival time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.packets++
	s.lastSeq = seq

	if s.haveLast {
		gapMs := arrival.Sub(s.lastArrival).Seconds() * 1000
		if gapMs > s.maxGapMs {
			s.maxGapMs = gapMs
		}
		s.insertGap(gap{seq: seq, gapMs: gapMs, atUnixMs: arrival.UnixMilli()})
		if gapMs > logGapThresholdMs {
			log.Printf("observer: gap %.2fms before seq=%d", gapMs, seq)
		}
	}

	s.haveLast = true
	s.lastArrival = arrival
}

// insertGap keeps topGaps sorted descending by gapMs, capped at
// topGapsLimit. Caller must hold s.mu.
func (s *stats) insertGap(g gap) {
	s.topGaps = append(s.topGaps, g)
	sort.Slice(s.topGaps, func(i, j int) bool { return s.topGaps[i].gapMs > s.topGaps[j].gapMs })
	if len(s.topGaps) > topGapsLimit {
		s.topGaps = s.topGaps[:topGapsLimit]
	}
}

// reset zeroes all accumulated stats and restarts the "since" clock.
// Fields are cleared individually (rather than *s = stats{...}) so the
// held mutex is never itself overwritten out from under its deferred
// Unlock.
func (s *stats) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.packets = 0
	s.haveLast = false
	s.lastArrival = time.Time{}
	s.lastSeq = 0
	s.maxGapMs = 0
	s.topGaps = nil
	s.since = time.Now()
}

// reportGap is the wire shape of one entry in report.TopGaps.
type reportGap struct {
	Seq      uint64  `json:"seq"`
	GapMs    float64 `json:"gap_ms"`
	AtUnixMs int64   `json:"at_unix_ms"`
}

// report is the wire shape of GET /report.
type report struct {
	Packets     uint64      `json:"packets"`
	MaxGapMs    float64     `json:"max_gap_ms"`
	TopGaps     []reportGap `json:"top_gaps"`
	LastSeq     uint64      `json:"last_seq"`
	SinceUnixMs int64       `json:"since_unix_ms"`
}

func (s *stats) report() report {
	s.mu.Lock()
	defer s.mu.Unlock()

	gaps := make([]reportGap, len(s.topGaps))
	for i, g := range s.topGaps {
		gaps[i] = reportGap{Seq: g.seq, GapMs: g.gapMs, AtUnixMs: g.atUnixMs}
	}
	return report{
		Packets:     s.packets,
		MaxGapMs:    s.maxGapMs,
		TopGaps:     gaps,
		LastSeq:     s.lastSeq,
		SinceUnixMs: s.since.UnixMilli(),
	}
}

// listenUDP runs the beacon receive loop until it hits an
// unrecoverable listener error.
func listenUDP(s *stats) error {
	conn, err := net.ListenPacket("udp", udpAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	buf := make([]byte, 1500)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			log.Printf("observer: udp read error: %v", err)
			continue
		}
		arrival := time.Now()
		if n < beaconPacketSize {
			continue
		}
		seq := binary.LittleEndian.Uint64(buf[0:8])
		s.record(seq, arrival)
	}
}

func main() {
	s := newStats()

	go func() {
		if err := listenUDP(s); err != nil {
			log.Fatalf("observer: udp listener failed: %v", err)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /report", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(s.report()); err != nil {
			log.Printf("observer: encode report: %v", err)
		}
	})
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		s.reset()
		w.WriteHeader(http.StatusNoContent)
	})

	log.Printf("observer: listening udp%s http%s", udpAddr, httpAddr)
	if err := http.ListenAndServe(httpAddr, mux); err != nil {
		log.Fatalf("observer: http server failed: %v", err)
	}
}
