// Command beacon is the guest liveness workload. It runs as the
// guest's PID 1 (kernel arg init=/init) and does nothing but send a
// UDP heartbeat to the observer every 2ms, forever.
//
// PID 1 exiting panics the kernel, so this program is built around one
// rule: never return from main. Every failure path retries with
// backoff instead of propagating an error upward.
//
// Built with CGO_ENABLED=0 GOOS=linux GOARCH=arm64. Stdlib only — the
// raw syscalls below (loopback bring-up, CLOCK_MONOTONIC read) exist
// so we don't need golang.org/x/sys/unix or a shell/ip binary in the
// rootfs, which contains nothing but this binary.
package main

import (
	"encoding/binary"
	"log"
	"net"
	"syscall"
	"time"
	"unsafe"
)

const (
	// beaconTarget is the observer's fixed address on fcnet (see
	// fc-contract.md Topology).
	beaconTarget = "172.30.0.20:9999"

	// beaconInterval is the steady-state send rate.
	beaconInterval = 2 * time.Millisecond

	// clockMonotonic is Linux's CLOCK_MONOTONIC id (time.h), not
	// exported by the stdlib syscall package.
	clockMonotonic = 1

	dialBackoffInitial = 10 * time.Millisecond
	dialBackoffMax     = 500 * time.Millisecond

	panicBackoff = 50 * time.Millisecond
)

// ifreqFlags is the subset of Linux's struct ifreq used by
// SIOCGIFFLAGS/SIOCSIFFLAGS: a 16-byte interface name followed by the
// ifr_flags union member, padded to the platform's 40-byte ifreq size.
type ifreqFlags struct {
	name  [16]byte
	flags int16
	_     [22]byte
}

// bringUpLoopback sets IFF_UP|IFF_RUNNING on "lo" via raw ioctls. It
// is best-effort: outbound UDP to a routed peer does not depend on lo,
// so a failure here is logged and otherwise ignored rather than
// retried or treated as fatal.
func bringUpLoopback() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	var ifr ifreqFlags
	copy(ifr.name[:], "lo")

	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCGIFFLAGS, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return errno
	}
	ifr.flags |= syscall.IFF_UP | syscall.IFF_RUNNING
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), syscall.SIOCSIFFLAGS, uintptr(unsafe.Pointer(&ifr))); errno != 0 {
		return errno
	}
	return nil
}

// monotonicNanos reads CLOCK_MONOTONIC directly via syscall, since the
// stdlib syscall package (unlike golang.org/x/sys/unix) does not wrap
// clock_gettime.
func monotonicNanos() uint64 {
	var ts syscall.Timespec
	if _, _, errno := syscall.Syscall(syscall.SYS_CLOCK_GETTIME, clockMonotonic, uintptr(unsafe.Pointer(&ts)), 0); errno != 0 {
		return 0
	}
	return uint64(ts.Sec)*1e9 + uint64(ts.Nsec)
}

// dialBeacon blocks until a UDP "connection" to the observer is
// established, retrying with capped exponential backoff. UDP dial can
// fail early in boot before eth0 has a route (ENETUNREACH); it always
// eventually succeeds once the kernel ip= configuration lands.
func dialBeacon() net.Conn {
	backoff := dialBackoffInitial
	for {
		conn, err := net.Dial("udp", beaconTarget)
		if err == nil {
			return conn
		}
		log.Printf("beacon: dial %s failed, retrying in %s: %v", beaconTarget, backoff, err)
		time.Sleep(backoff)
		backoff *= 2
		if backoff > dialBackoffMax {
			backoff = dialBackoffMax
		}
	}
}

// run drives the heartbeat loop. It only returns on an unrecoverable
// panic, which main() catches and restarts.
func run() {
	if err := bringUpLoopback(); err != nil {
		log.Printf("beacon: bring up lo: %v", err)
	}

	conn := dialBeacon()
	defer conn.Close()

	ticker := time.NewTicker(beaconInterval)
	defer ticker.Stop()

	var seq uint64
	var buf [16]byte
	for range ticker.C {
		binary.LittleEndian.PutUint64(buf[0:8], seq)
		binary.LittleEndian.PutUint64(buf[8:16], monotonicNanos())

		if _, err := conn.Write(buf[:]); err != nil {
			log.Printf("beacon: send seq=%d failed, redialing: %v", seq, err)
			conn.Close()
			conn = dialBeacon()
			continue
		}
		seq++
	}
}

func main() {
	// PID 1 must never exit. run() only returns via panic (an
	// unexpected bug, not an expected failure mode — those are all
	// handled inside run()/dialBeacon() with retries); recover and
	// restart rather than let the process die.
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("beacon: recovered panic, restarting: %v", r)
					time.Sleep(panicBackoff)
				}
			}()
			run()
		}()
	}
}
