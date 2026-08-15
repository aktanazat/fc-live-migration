# fc-live-migration

Live migration of a Firecracker microVM between two hosts, with a
network client watching the whole time. Blackout target: 30ms or
less. Dedalus Labs take-home.

## What this is

`hostd` runs on each host and exposes a REST API over the Firecracker
process it manages: boot, snapshot, push files to a peer, load, pause,
resume, cutover. `migratectl` is the orchestrator: it drives two
`hostd` instances through a pre-copy migration and reports how long
the guest was actually frozen. A guest process (`beacon`) sends a UDP
packet every 2ms so an independent `observer` can measure the real
gap, not just what the orchestrator claims.

Everything runs on one Mac. The two "hosts" are Docker containers on
the same bridge networks, each with `/dev/kvm` passed through from a
nested-virtualization Lima VM. This is a stand-in for two physical
machines: same subnet, same MAC/IP scheme, real Firecracker snapshot
files moved over a real, if short, network hop.

## Architecture

```
Mac (Apple Silicon, macOS 15+)
 └─ Lima VM "fcdev"  (vz, nested virtualization → /dev/kvm)
      ├─ Docker network "fcnet"   172.30.0.0/24  (guest + beacon traffic)
      ├─ Docker network "mignet"  172.31.0.0/24  (snapshot pushes + cutover)
      │
      ├─ host-a   fcnet 172.30.0.11  mignet 172.31.0.11  hostd :8080 (127.0.0.1:8081)
      │     Firecracker ── guest vm0
      │        NIC 172.30.0.50 / AA:FC:00:00:00:01
      │        tap0 ─┬─ br0 ── eth0 (host-a's fcnet IP)
      │
      ├─ host-b   fcnet 172.30.0.12  mignet 172.31.0.12  hostd :8080 (127.0.0.1:8082)
      │     Firecracker ── guest vm0 (prepared, then migration target)
      │        tap0 ─┬─ br0 ── eth0 (host-b's fcnet IP)
      │
      └─ observer 172.30.0.20   :9090 (127.0.0.1:9090)
            UDP :9999 ← beacon packets from vm0

      migratectl runs on the Lima VM host, talks to hostd over
      127.0.0.1:8081 / 127.0.0.1:8082 (compose-published ports). hostd
      itself talks to its peer over mignet, container to container, so
      snapshot bytes never share a wire with the guest's own traffic.
```

Each host is on both networks. fcnet carries the guest's own packets
and the beacon; the guest keeps its IP and MAC across the move, only
which host's tap/bridge it is plugged into changes. mignet exists so
a multi-megabyte snapshot push never queues ahead of a beacon packet
on the same bridge; this is the same reason production hypervisors
give live migration a dedicated NIC.

vm0 can move either direction. A migration first checks whether vm0
is already running on the source (the reverse leg of an earlier
migration lands here) and only boots it if it is not. `scripts/demo.sh`
exercises both legs: host-a to host-b, then the same running guest
back from host-b to host-a.

## Base checkpoint invariant

Every VM hostd can migrate maintains one invariant: `base/mem` under
its snapshot directory equals guest memory as of the VM's last
snapshot or load, and the Firecracker dirty-page bitmap tracks writes
since then.

- A freshly booted VM gets a full snapshot at provisioning time,
  before any workload runs, while `hostd` still owns it exclusively.
  This bounds every later migration's diffs to what the workload
  actually dirtied, never to the full guest memory size.
- A VM restored on a target becomes the new source of truth for its
  own base: the memory file it was loaded from is recorded as
  `base/mem` directly.
- Every diff snapshot taken afterward is merged into the local base
  in place (`sparse.Merge`) after the guest resumes, so the merge
  cost never lands inside a blackout window.

This is why base sync (below) can ship the whole checkpoint, however
large, with the guest running: `base/mem` is never stale by more than
one migration.

## How the migration works

`migratectl` drives the whole thing. `hostd` never talks to its peer
except when told to.

1. **Reuse or boot.** `migratectl` checks whether vm0 is already
   running on the source. If so it migrates that guest as-is
   (reverse leg). Otherwise `POST host-a /vms` boots it fresh with
   `track_dirty_pages` on, which also takes the base snapshot above.
2. **Prepare.** `POST host-b /vms/prepare` spawns a Firecracker
   process on the target and brings its API socket up, but boots
   nothing. This gets process startup cost out of the way before the
   migration starts, so it never counts against blackout.
3. **Settle (optional).** `migratectl` waits for the beacon to reach
   steady state (packet count above a threshold and still advancing),
   then resets the observer. This makes sure the migration measures a
   guest already running its workload, not one still booting, and
   that the observer's report covers only the migration window.
4. **Base sync.** Source pushes its standing base (`base/mem` as
   sparse extents, plus `base/state`) to the target over mignet while
   the guest keeps running. This is the bulk of the guest's RAM and
   it can take as long as it needs; the guest is never paused for it.
5. **Diff rounds.** While the guest ran during base sync, it dirtied
   some pages. Round 1+ takes a diff snapshot of only those pages,
   which Firecracker returns as a sparse file; `hostd` walks it with
   `SEEK_DATA`/`SEEK_HOLE`, sends only the allocated extents, and the
   target `pwrite`s them onto `base/mem` in place. Firecracker resets
   the dirty-page bitmap on every snapshot it takes, diff included,
   so every diff snapshot must be pushed before the next one is
   requested: an unpushed diff's pages would vanish from both the old
   base and the bitmap and be unrecoverable (see Pitfalls below). Each
   round is smaller than the last, because there is less time between
   snapshots for the guest to dirty new pages. This repeats until a
   round's diff drops at or under a threshold (1 MiB default) or a
   round cap (8) is hit, whichever comes first.
6. **Cutover.** This is the only part where the guest is not running.
   Source `hostd`, in one call over pre-warmed keep-alive connections
   to the peer: pause the vCPU, take one last diff snapshot (now
   bounded by the last round interval, so it is small), push its
   extents plus final vmstate onto the target's `base/mem`, then call
   the target's `/load` with `resume=true`. Blackout is measured from
   pause to the moment the target acknowledges the guest resumed. If
   any step after the pause fails, the source resumes the guest and
   folds the already-taken final diff into its own base before
   returning the error, so the dirty pages it already consumed are
   not lost on a retry.
7. **Teardown.** Source `DELETE`s vm0. It has no more state worth
   keeping; the guest's authoritative copy is now on the target.

Blackout is exactly step 6, nothing else. Everything before it
happens with the guest live.

### Why 30ms is achievable

- **The final diff is tiny.** By the time cutover runs, prior diff
  rounds have already converged the target's memory to within one
  round interval of the source. The last diff only has to cover pages
  dirtied in the time between the last pre-copy round and the pause.
- **The link is fast and local.** host-a and host-b are containers on
  the same Docker bridges, on the same machine. There is no real WAN
  hop, no queuing, no packet loss, and bulk transfer has mignet to
  itself.
- **The target process is already warm.** `/vms/prepare` runs before
  the pre-copy loop even starts. Firecracker's own process spin-up
  and API socket bind happen outside the timed window. Cutover only
  has to load a snapshot into an already-running process, which is
  fast and deterministic.
- **The guest's identity does not change.** Same MAC, same IP, same
  ARP entry on the bridge. A TCP peer mid-connection would see the
  stream stall for the blackout window and then continue: no RST, no
  reconnect, no DNS lookup, because nothing about the guest's network
  identity moved. The UDP beacon in this repo shows the same thing as
  a clean gap in sequence numbers, not a connection failure.

None of these hold the budget on their own. A fast link with a cold
target process, or a warm target with a huge final diff, would both
blow it. The combination is what keeps blackout under 30ms.

## Measured result

One run, this repo, 2026-08-15, on the environment below.

**host-a → host-b** (fresh boot, first migration):

| | |
|---|---|
| round 1 diff | 27.4MB, snapshot 8.55ms |
| round 2 diff | 0.7MB, snapshot 2.90ms |
| cutover | pause 0.218ms, snapshot 1.511ms, push 1.455ms, load 3.940ms → 7.125ms |
| **total_blackout_ms** | **18.580** |
| observer max_gap_ms | 22.25 |

**host-b → host-a** (same running guest, reverse leg):

| | |
|---|---|
| cutover | **6.780ms** |
| **total_blackout_ms** | **9.889** |
| observer max_gap_ms | 2.877 |

Both legs pass: total blackout and observer max gap are both under
30ms. The guest is reachable and alive after each move (ping answers,
beacon resumes at roughly 500 packets/second).

## Quickstart

Run this from the Mac, in the repo root, in order:

```sh
limactl start lima/fcdev.yaml
limactl shell fcdev
```

Everything past this point runs inside the `fcdev` Lima VM shell,
from the mounted repo root (`~/Developer/fc-live-migration` on the
Mac is the same path inside the VM):

```sh
make kernel      # fetch a prebuilt aarch64 vmlinux into artifacts/
make rootfs      # build a minimal ext4 rootfs containing the beacon
make build       # go build hostd, observer (linux/arm64) and migratectl (host)
make images      # docker build the host-a/host-b image
make up          # docker compose up: host-a, host-b, observer
make migrate     # run one migration, vm0: host-a -> host-b
make demo        # scripted end-to-end run for recording, see below
```

`make down` tears the containers down. `make clean` removes
`artifacts/` and `bin/`.

### migratectl flags worth knowing

- `--peer` is the target hostd's URL as reachable from inside the
  *source container*, not the localhost port migratectl itself talks
  to. `--target` is usually a compose-published `127.0.0.1:808x` port
  for the orchestrator's own use; `hostd` needs the peer's real mignet
  address (default `http://172.31.0.12:8080`, host-b's mignet IP) to
  push bytes and issue the cutover load host-to-host. Migrating the
  other direction needs the other host's mignet address, e.g.
  `--peer http://172.31.0.11:8080` for host-b → host-a.
- `--settle-packets` (default 500) is the beacon packet count the
  settle step waits for before starting the migration and resetting
  the observer. Raise it if the workload takes longer to reach steady
  state; lower it (or pass `--no-observer`) to skip the wait entirely.
- `--threshold-bytes` (default 1MiB) and `--max-rounds` (default 8)
  bound the pre-copy loop: it stops early once a diff round's data
  falls at or under the threshold, or after the round cap, whichever
  comes first.

## hostd API

All requests and responses are JSON except the two file endpoints.
Full field-level detail is in `internal/api/types.go`, which is the
source of truth.

| Method | Path | Request | Response | What it does |
|---|---|---|---|---|
| GET | `/healthz` | none | `{"ok":true}` | liveness check |
| POST | `/vms` | `CreateVMRequest` | 201 `VMInfo` | boot a fresh microVM, then take its base snapshot |
| POST | `/vms/prepare` | `PrepareRequest` | 201 `VMInfo` | spawn Firecracker + tap, no boot; awaits `/load` |
| GET | `/vms/{id}` | none | `VMInfo` | current state of a VM |
| DELETE | `/vms/{id}` | none | 204 | kill Firecracker, remove state |
| POST | `/vms/{id}/pause` | none | `OpTiming` | pause the vCPU |
| POST | `/vms/{id}/resume` | none | `OpTiming` | resume the vCPU |
| POST | `/vms/{id}/snapshot` | `SnapshotRequest` | `SnapshotResponse` | full or diff snapshot; diffs merge into the local base |
| POST | `/vms/{id}/push` | `PushRequest` | `PushResponse` | stream local files to a peer hostd |
| POST | `/vms/{id}/load` | `LoadRequest` | `OpTiming` | load a snapshot into a prepared VM |
| POST | `/vms/{id}/cutover` | `CutoverRequest` | `CutoverResponse` | pause, final diff, push, remote load; source side only |
| POST | `/files/base?dir=&name=` | octet-stream | `FileWriteResponse` | receive one whole file |
| POST | `/files/extents?dir=&name=&size=` | extent stream | `FileWriteResponse` | apply sparse extents to a file in place |

Extent wire format (`/files/extents` body, and the sparse half of
`/push`): repeated frames of `[offset uint64 LE][length uint64 LE]
[length bytes of data]` until EOF. The receiver `pwrite`s each frame
at its offset into the named file, then truncates it to the `size`
query parameter so trailing holes survive the transfer.

### migratectl

```sh
bin/migratectl boot --host http://127.0.0.1:8081 --vm smoke0 \
  --kernel artifacts/vmlinux --rootfs artifacts/rootfs.ext4

bin/migratectl migrate --source http://127.0.0.1:8081 --target http://127.0.0.1:8082
```

`boot` sends a `CreateVMRequest` to `--host` and prints
`booted <id> on <host>: state=... pid=...`. It exists for smoke-
testing a host independent of a full migration; `scripts/demo.sh`
uses it exactly that way, on a throwaway id, before the real run.

`migrate` runs the full algorithm above end to end against `--source`
and `--target`: it reuses or boots vm0 itself (step 1), prepares the
target, settles, runs the pre-copy rounds, executes cutover, deletes
the source copy, and on success prints one final greppable line:

```
RESULT total_blackout_ms=<float> cutover_blackout_ms=<float> extents=<int> diff_bytes=<int> max_gap_ms=<float> pass=<true|false>
```

`max_gap_ms` comes from `migrate` querying the observer itself (`-1`
if `--no-observer` was passed); `pass` requires both
`total_blackout_ms` and `max_gap_ms` to be at or under the 30ms
budget. Process exit code is nonzero on any migration error, but note
that a completed migration with `pass=false` still exits 0: the
budget is a report, not a hard failure.

## Measuring blackout

Two independent numbers, both printed by `migratectl migrate` and
both checked by `scripts/demo.sh`:

1. **Source-clock total blackout.** `MigrationReport.TotalBlackoutMs`
   sums every window in which the guest was actually frozen: each
   diff round's pause+snapshot+resume plus the final cutover blackout
   (`CutoverResponse.BlackoutMs`, pause to remote resume
   acknowledged). This is not just the cutover number; a diff round
   also pauses the guest for the moment its snapshot is written, and
   those pauses count too. It is the more optimistic of the two
   measurements, since it never leaves host-a's own clock.
2. **Independent observer gap.** The `observer` container has no part
   in the migration. It just timestamps UDP packet arrivals from vm0
   on 172.30.0.20:9999 and computes the largest inter-arrival gap
   over the run (`GET /report` → `max_gap_ms`). Steady-state gap
   between beacon packets is about 2ms, so anything meaningfully
   above that is the blackout window as observed by a third party on
   the network. This is the number that actually matters, because it
   does not trust the thing being measured to grade itself.

A run counts as a pass only if both numbers are at or under 30ms.

## Design decisions

- **RAM moved host-to-host, not through the orchestrator.** `push`
  streams directly from source hostd to target hostd; migratectl only
  issues control calls and never touches snapshot bytes. This mirrors
  how real hypervisor migration works and keeps the orchestrator off
  the data path, so its own overhead cannot leak into the blackout
  number.
- **A dedicated migration network.** Snapshot pushes and the cutover
  load run over mignet (172.31.0.0/24), separate from fcnet
  (172.30.0.0/24), which carries the guest's own traffic and the
  beacon. A multi-megabyte base push shares no wire with a 2ms UDP
  beacon.
- **Docker containers as "hosts".** Two full VMs would also work but
  would need nested nested-virtualization on the Lima VM, which is
  not reliably supported. Two privileged containers sharing the Lima
  VM's kernel and `/dev/kvm` get the same process and network
  isolation that matters for this exercise (each host is its own
  Firecracker process, own tap, own IP) without a second layer of
  virtualization.
- **Firecracker pinned to v1.14.4.** Both containers run the exact
  same build from the same release tarball (`FC_SOURCE_BUILD=1`
  builds it from a clone instead, same version). Snapshot
  compatibility across Firecracker versions is not guaranteed, and
  version drift between source and target would be an easy way to
  fail migration for reasons that have nothing to do with the
  algorithm.
- **aarch64 caveats.** This all runs on Apple Silicon, so every
  Firecracker instance is aarch64:
  - No CPU templates on aarch64. None are configured, and none are
    needed for a same-host-generation migration.
  - The GIC (interrupt controller) state in a snapshot must match the
    GIC of the VM it is restored into. Since host-a and host-b are
    both plain KVM VMs backed by the same Lima VM's CPU, this holds
    automatically. It would need explicit handling on real
    heterogeneous hardware.
  - Diff snapshots on aarch64 were a developer-preview feature as of
    v1.14.4. They work as documented here, but are called out because
    they are the newer, less-hardened half of the snapshot API
    compared to full snapshots.
  - The guest clock steps forward at restore, since the VM was frozen
    on one host's clock and resumed on another's. The beacon's own
    sequence numbers, not wall time, are what the observer trusts.

## Pitfalls found during integration

- **An unpushed diff snapshot loses pages, permanently.** Firecracker
  resets the dirty-page bitmap on every snapshot call, diff included,
  not just full ones. Early on, a code path took a diff snapshot,
  hit an error before the push completed, and moved on without
  retrying the push. Those pages were now gone from both the old base
  (never merged in) and the bitmap (already reset), so no later diff
  or base sync could recover them; the target's restored guest was
  silently running on stale memory in that range. It surfaced as a
  guest that migrated cleanly and then behaved wrong. It was root-
  caused by byte-comparing a full snapshot against the local base a
  diff round claimed to have merged, and by hashing the mem file on
  both sides of the wire at each push. The fix has two parts: the
  pre-copy loop's convergence check only runs after a diff's push
  succeeds, and cutover's failure path folds the final diff into the
  local base before returning an error, so a retried migration never
  starts from a base that is missing pages it already paid the dirty-
  bitmap cost for.
- **The orchestrator's URL and the peer's URL are not the same URL.**
  `migratectl --target` is a compose-published `127.0.0.1:808x` port,
  reachable from the Lima VM shell where migratectl runs. But
  `hostd`'s push and cutover calls happen from inside the *source
  container*, where `127.0.0.1` means the source container, not the
  target. Passing the orchestrator's `--target` value straight through
  to `hostd` sent every snapshot byte nowhere. The fix is the separate
  `--peer` flag: the target's real address on mignet, as seen from a
  container, not from the Mac or the Lima VM shell.
- **UDP contention before mignet existed.** With snapshot pushes and
  beacon packets sharing one bridge network, a multi-megabyte base
  push could queue ahead of the 2ms beacon on the same interface,
  showing up as an observer gap much larger than what the source
  clock's cutover timing reported for the same run. Splitting bulk
  transfer onto its own network (mignet) removed the contention; it
  is the same reason production hypervisors give live migration a
  dedicated NIC rather than sharing the guest's own link.

## Limitations

- No authentication or TLS on the hostd API. It listens only inside
  the Docker network, which is the intended trust boundary here.
- No retry or rollback across a fully failed migration once the
  source VM has been deleted; rollback only covers a failed cutover
  attempt before that point.
- The beacon is a synthetic liveness signal, a UDP counter, not a real
  application. It proves the guest was frozen for X ms and nothing
  else. It says nothing about, for example, how a stateful TCP server
  would behave mid-request.
- Diff round count and size threshold are fixed defaults, not tuned
  per workload. A guest that writes memory faster than this one would
  need different tuning to keep the final diff small. A long-idle
  guest between migrations also accumulates more dirt than a busy
  one, which would widen the first diff round's pause; the base
  checkpoint invariant already supports periodic background
  checkpoints to bound that, but this repo does not schedule any.
- Rootfs is shared, read-only storage: the same `/artifacts/rootfs.ext4`
  path is mounted into both containers, and the kernel boots it with
  `ro`. This repo migrates guest RAM, not disk state; a workload that
  needs writable local disk would need a real block-device migration
  path, which is out of scope here.