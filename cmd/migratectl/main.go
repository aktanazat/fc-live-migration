// Command migratectl drives Firecracker live migration between two
// hostd instances and reports on the resulting blackout.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/aktanazat/fc-live-migration/internal/api"
	"github.com/aktanazat/fc-live-migration/internal/migrate"
)

const (
	defaultKernelArgs  = "ro console=ttyS0 reboot=k panic=1 pci=off init=/init ip=172.30.0.50::172.30.0.1:255.255.255.0:fcguest:eth0:off"
	defaultTapName     = "tap0"
	defaultTapMAC      = "AA:FC:00:00:00:01"
	defaultVMID        = "vm0"
	defaultSourceURL   = "http://127.0.0.1:8081"
	defaultTargetURL   = "http://127.0.0.1:8082"
	defaultPeerURL     = "http://172.31.0.12:8080"
	defaultObserverURL = "http://127.0.0.1:9090"
	defaultKernelPath  = "/artifacts/vmlinux"
	defaultRootfsPath  = "/artifacts/rootfs.ext4"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}

	var err error
	switch os.Args[1] {
	case "boot":
		err = runBoot(os.Args[2:])
	case "migrate":
		err = runMigrate(os.Args[2:])
	case "status":
		err = runStatus(os.Args[2:])
	case "report":
		err = runReport(os.Args[2:])
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "migratectl: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "migratectl:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `migratectl controls Firecracker live migration between hostd instances.

Usage:
  migratectl boot [flags]      boot a microVM on one hostd host
  migratectl migrate [flags]   run the full precopy + cutover migration
  migratectl status [flags]    query VM state on the source and target hosts
  migratectl report [flags]    fetch the observer's blackout report

Run "migratectl <subcommand> -h" for flag details.
`)
}

func runBoot(args []string) error {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	host := fs.String("host", defaultSourceURL, "hostd base URL to boot on")
	id := fs.String("vm", defaultVMID, "VM id")
	kernel := fs.String("kernel", defaultKernelPath, "kernel image path (container-local)")
	rootfs := fs.String("rootfs", defaultRootfsPath, "rootfs image path (container-local)")
	kernelArgs := fs.String("kernel-args", defaultKernelArgs, "kernel boot args")
	vcpus := fs.Int64("vcpus", 1, "vCPU count")
	memMiB := fs.Int64("mem-mib", 128, "guest memory in MiB")
	tapName := fs.String("tap-name", defaultTapName, "guest tap device name")
	tapMAC := fs.String("tap-mac", defaultTapMAC, "guest MAC address")
	trackDirty := fs.Bool("track-dirty-pages", true, "enable dirty page tracking")
	timeout := fs.Duration("timeout", 30*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	client := migrate.NewClient(*host)
	info, err := client.CreateVM(ctx, api.CreateVMRequest{
		ID:              *id,
		KernelPath:      *kernel,
		RootfsPath:      *rootfs,
		KernelArgs:      *kernelArgs,
		VCPUs:           *vcpus,
		MemMiB:          *memMiB,
		Tap:             api.TapConfig{Name: *tapName, GuestMAC: *tapMAC},
		TrackDirtyPages: *trackDirty,
	})
	if err != nil {
		return fmt.Errorf("boot: %w", err)
	}
	fmt.Printf("booted %s on %s: state=%s pid=%d\n", info.ID, *host, info.State, info.PID)
	return nil
}

func runMigrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ExitOnError)
	source := fs.String("source", defaultSourceURL, "source hostd base URL")
	target := fs.String("target", defaultTargetURL, "target hostd base URL")
	peer := fs.String("peer", defaultPeerURL, "target hostd base URL as reachable from the source container")
	id := fs.String("vm", defaultVMID, "VM id")
	kernel := fs.String("kernel", defaultKernelPath, "kernel image path (container-local)")
	rootfs := fs.String("rootfs", defaultRootfsPath, "rootfs image path (container-local)")
	kernelArgs := fs.String("kernel-args", defaultKernelArgs, "kernel boot args")
	vcpus := fs.Int64("vcpus", 1, "vCPU count")
	memMiB := fs.Int64("mem-mib", 128, "guest memory in MiB")
	tapName := fs.String("tap-name", defaultTapName, "guest tap device name")
	tapMAC := fs.String("tap-mac", defaultTapMAC, "guest MAC address")
	thresholdBytes := fs.Int64("threshold-bytes", migrate.DefaultThresholdBytes, "stop precopy once a diff round's data bytes fall at or below this")
	maxRounds := fs.Int("max-rounds", migrate.DefaultMaxRounds, "maximum diff precopy rounds before cutover")
	jsonOut := fs.Bool("json", false, "print the migration report as JSON")
	observerURL := fs.String("observer", defaultObserverURL, "observer base URL")
	noObserver := fs.Bool("no-observer", false, "skip the observer reset/report round trip")
	settleMinPackets := fs.Int64("settle-packets", 500, "beacon packets required before migrating (observer settle)")
	timeout := fs.Duration("timeout", 2*time.Minute, "overall migration timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	var obsClient *migrate.ObserverClient
	var settle func(context.Context) error
	if !*noObserver {
		obsClient = migrate.NewObserverClient(*observerURL)
		if err := obsClient.Reset(ctx); err != nil {
			return fmt.Errorf("observer reset: %w", err)
		}
		// Migrate a guest in steady state, not one still booting:
		// wait for the beacon to flow, then reset the observer so
		// its report covers exactly the migration window.
		settle = func(ctx context.Context) error {
			return obsClient.SettleWait(ctx, *settleMinPackets)
		}
	}

	cfg := migrate.Config{
		VMID:           *id,
		SourceURL:      *source,
		TargetURL:      *target,
		PeerURL:        *peer,
		KernelPath:     *kernel,
		RootfsPath:     *rootfs,
		KernelArgs:     *kernelArgs,
		VCPUs:          *vcpus,
		MemMiB:         *memMiB,
		Tap:            api.TapConfig{Name: *tapName, GuestMAC: *tapMAC},
		ThresholdBytes: *thresholdBytes,
		MaxRounds:      *maxRounds,
		Settle:         settle,
	}
	report, err := migrate.Orchestrate(ctx, cfg)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	var obs migrate.ObserverReport
	haveObs := false
	if obsClient != nil {
		obs, err = obsClient.Report(ctx)
		if err != nil {
			return fmt.Errorf("observer report: %w", err)
		}
		haveObs = true
	}

	if *jsonOut {
		if err := writeMigrateJSON(os.Stdout, report, obs, haveObs); err != nil {
			return err
		}
	} else {
		writeMigrateHuman(os.Stdout, report, obs, haveObs)
	}
	printResultLine(os.Stdout, report, obs, haveObs)
	return nil
}

func writeMigrateHuman(w io.Writer, r *migrate.MigrationReport, obs migrate.ObserverReport, haveObs bool) {
	r.WriteHuman(w)
	if haveObs {
		fmt.Fprintf(w, "observer: packets=%d max_gap_ms=%.3f last_seq=%d\n", obs.Packets, obs.MaxGapMs, obs.LastSeq)
	}

	pass := migratePass(r, obs, haveObs)
	status := "FAIL"
	if pass {
		status = "PASS"
	}
	if haveObs {
		fmt.Fprintf(w, "verdict: %s (<=%.0fms)  total_blackout_ms=%.3f cutover_blackout_ms=%.3f observer_max_gap_ms=%.3f\n",
			status, migrate.BlackoutThresholdMs, r.TotalBlackoutMs, r.Cutover.BlackoutMs, obs.MaxGapMs)
	} else {
		fmt.Fprintf(w, "verdict: %s (<=%.0fms)  total_blackout_ms=%.3f cutover_blackout_ms=%.3f (observer skipped)\n",
			status, migrate.BlackoutThresholdMs, r.TotalBlackoutMs, r.Cutover.BlackoutMs)
	}
}

func writeMigrateJSON(w io.Writer, r *migrate.MigrationReport, obs migrate.ObserverReport, haveObs bool) error {
	out := struct {
		Migration *migrate.MigrationReport `json:"migration"`
		Observer  *migrate.ObserverReport  `json:"observer,omitempty"`
		Pass      bool                     `json:"pass"`
	}{
		Migration: r,
		Pass:      migratePass(r, obs, haveObs),
	}
	if haveObs {
		out.Observer = &obs
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// migratePass applies the challenge bar to the strictest available
// numbers: the summed guest-frozen time on the source clock, and the
// worst beacon inter-arrival gap the observer saw on the wire.
func migratePass(r *migrate.MigrationReport, obs migrate.ObserverReport, haveObs bool) bool {
	pass := migrate.Passes(r.TotalBlackoutMs)
	if haveObs {
		pass = pass && migrate.Passes(obs.MaxGapMs)
	}
	return pass
}

// printResultLine emits one final greppable line summarizing the
// outcome, for scripts (e.g. demo.sh) to parse without depending on
// --json output shape.
func printResultLine(w io.Writer, r *migrate.MigrationReport, obs migrate.ObserverReport, haveObs bool) {
	maxGap := -1.0
	if haveObs {
		maxGap = obs.MaxGapMs
	}
	fmt.Fprintf(w, "RESULT total_blackout_ms=%.3f cutover_blackout_ms=%.3f extents=%d diff_bytes=%d max_gap_ms=%.3f pass=%t\n",
		r.TotalBlackoutMs, r.Cutover.BlackoutMs, r.Cutover.Extents, r.Cutover.DiffBytes, maxGap, migratePass(r, obs, haveObs))
}

func runStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	id := fs.String("vm", defaultVMID, "VM id")
	source := fs.String("source", defaultSourceURL, "source hostd base URL")
	target := fs.String("target", defaultTargetURL, "target hostd base URL")
	timeout := fs.Duration("timeout", 15*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	srcInfo, srcErr := migrate.NewClient(*source).GetVM(ctx, *id)
	tgtInfo, tgtErr := migrate.NewClient(*target).GetVM(ctx, *id)

	print := func(label, url string, info api.VMInfo, err error) {
		switch {
		case err == nil:
			fmt.Printf("%-6s %-24s id=%s state=%-9s pid=%d\n", label, url, info.ID, info.State, info.PID)
		case errors.Is(err, migrate.ErrNotFound):
			fmt.Printf("%-6s %-24s no %s here\n", label, url, *id)
		default:
			fmt.Printf("%-6s %-24s error: %v\n", label, url, err)
		}
	}
	print("source", *source, srcInfo, srcErr)
	print("target", *target, tgtInfo, tgtErr)

	notFound := func(err error) bool { return err == nil || errors.Is(err, migrate.ErrNotFound) }
	if !notFound(srcErr) || !notFound(tgtErr) {
		return fmt.Errorf("status query failed on at least one host")
	}
	return nil
}

func runReport(args []string) error {
	fs := flag.NewFlagSet("report", flag.ExitOnError)
	observerURL := fs.String("observer", defaultObserverURL, "observer base URL")
	jsonOut := fs.Bool("json", false, "print as JSON")
	timeout := fs.Duration("timeout", 15*time.Second, "request timeout")
	if err := fs.Parse(args); err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()

	rep, err := migrate.NewObserverClient(*observerURL).Report(ctx)
	if err != nil {
		return fmt.Errorf("observer report: %w", err)
	}

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}

	fmt.Printf("packets=%d max_gap_ms=%.3f last_seq=%d since=%s\n",
		rep.Packets, rep.MaxGapMs, rep.LastSeq,
		time.UnixMilli(rep.SinceUnixMs).Format(time.RFC3339))
	if len(rep.TopGaps) > 0 {
		fmt.Println("top gaps:")
		for _, g := range rep.TopGaps {
			fmt.Printf("  seq=%d gap_ms=%.3f at=%s\n", g.Seq, g.GapMs,
				time.UnixMilli(g.AtUnixMs).Format(time.RFC3339Nano))
		}
	}
	return nil
}
