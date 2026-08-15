# fc-live-migration

Live migration of a Firecracker microVM between two hosts, with a
network client watching the whole time. Blackout target: 30ms or
less. Dedalus Labs take-home.

## What this is

`hostd` runs on each host and exposes a REST API over the Firecracker
process it manages: boot, snapshot, push files to a peer, load, pause,
resume. `migratectl` is the orchestrator: it drives two `hostd`
instances through a pre-copy migration and reports how long the guest
was actually frozen. A guest process (`beacon`) sends a UDP packet
every 2ms so an independent `observer` can measure the real gap, not
just what the orchestrator claims.

Everything runs on one Mac. The two "hosts" are Docker containers on
the same bridge network, each with `/dev/kvm` passed through from a
nested-virtualization Lima VM. This is a stand-in for two physical
machines: same subnet, same MAC/IP scheme, real Firecracker snapshot
files moved over a real, if short, network hop.

## Architecture

```
Mac (Apple Silicon, macOS 15+)
 └─ Lima VM "fcdev"  (vz, nested virtualization → /dev/kvm)
      └─ Docker network "fcnet"  172.30.0.0/24
           │
           ├─ host-a   172.30.0.11   hostd :8080  (127.0.0.1:8081)
           │     Firecracker ── guest vm0 (running)
           │        NIC 172.30.0.50 / AA:FC:00:00:00:01
           │        tap0 ─┬─ br0 ── eth0 (host-a's container IP)
           │
           ├─ host-b   172.30.0.12   hostd :8080  (127.0.0.1:8082)
           │     Firecracker ── guest vm0 (prepared, then migration target)
           │        tap0 ─┬─ br0 ── eth0 (host-b's container IP)
           │
           └─ observer 172.30.0.20   :9090 (127.0.0.1:9090)
                 UDP :9999 ← beacon packets from vm0

           migratectl runs on the Lima VM host, talks to hostd over
           127.0.0.1:8081 / 127.0.0.1:8082 (compose-published ports).
```

The guest keeps its IP and MAC across the move. Only which host's
tap/bridge it is plugged into changes. From the observer's point of
view the beacon just goes quiet for a moment, then resumes from the
next sequence number: same source address, same stream.

## How the migration works

`migratectl` drives the whole thing. `hostd` never talks to its peer
except when told to.

1. **Boot.** `POST host-a /vms` starts vm0 with `track_dirty_pages`
   on. Firecracker tracks which guest memory pages have been written
   since the last snapshot.
2. **Prepare.** `POST host-b /vms/prepare` spawns a Firecracker
   process on the target and brings its API socket up, but boots
   nothing. This gets process startup cost out of the way before the
   migration starts, so it never counts against blackout.
3. **Base copy (round 0).** Source takes a full snapshot
   (`snapshot_type=Full`) while the guest keeps running, then pushes
   the memory file and VM state to the target as `base/mem` and
   `base/state`. This is the bulk of the guest's RAM (128 MiB here)
   and it can take as long as it needs. The guest is not paused for
   this step.
4. **Diff rounds.** While the guest ran during the base push, it
   dirtied some pages. Round 1+ snapshots only those pages
   (`snapshot_type=Diff`), which Firecracker returns as a sparse
   file: mostly holes, a few dirty regions. `hostd` walks it with
   `SEEK_DATA`/`SEEK_HOLE`, sends only the allocated extents, and the
   target `pwrite`s them onto `base/mem` in place. Each round is
   smaller than the last, because there is less time between
   snapshots for the guest to dirty new pages. This repeats until a
   round's diff drops under a threshold (1 MiB default) or a round
   cap (8) is hit, whichever comes first.
5. **Cutover.** This is the only part where the guest is not running.
   Source hostd, in one call: pause the vCPU, take one last diff
   snapshot (now bounded by the last round interval, so it is small),
   push its extents plus final vmstate onto the target's `base/mem`,
   then call the target's `/load` with `resume=true`. Blackout is
   measured from pause to the moment the target acknowledges the
   guest resumed.
6. **Teardown.** Source `DELETE`s vm0. It has no more state worth
   keeping.

Blackout is exactly step 5, nothing else. Everything before it
happens with the guest live.

### Why 30ms is achievable

- **The final diff is tiny.** By the time cutover runs, prior diff
  rounds have already converged the target's memory to within one
  round interval of the source. The last diff only has to cover pages
  dirtied in the time between the last pre-copy round and the pause.
  At 128 MiB of guest memory and a workload that touches a handful of
  pages per millisecond, that is tens of kilobytes, not megabytes.
- **The link is fast and local.** host-a and host-b are containers on
  the same Docker bridge, on the same machine. There is no real WAN
  hop, no queuing, no packet loss. Pushing a few dozen kilobytes over
  it takes well under a millisecond.
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

None of these four hold the budget on their own. A fast link with a
cold target process, or a warm target with a huge final diff, would
both blow it. The combination is what keeps blackout under 30ms.

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

## hostd API

All requests and responses are JSON except the two file endpoints.
Full field-level detail is in `internal/api/types.go`, which is the
source of truth.

| Method | Path | Request | Response | What it does |
|---|---|---|---|---|
| GET | `/healthz` | none | `{"ok":true}` | liveness check |
| POST | `/vms` | `CreateVMRequest` | 201 `VMInfo` | boot a fresh microVM |
| POST | `/vms/prepare` | `PrepareRequest` | 201 `VMInfo` | spawn Firecracker + tap, no boot; awaits `/load` |
| GET | `/vms/{id}` | none | `VMInfo` | current state of a VM |
| DELETE | `/vms/{id}` | none | 204 | kill Firecracker, remove state |
| POST | `/vms/{id}/pause` | none | `OpTiming` | pause the vCPU |
| POST | `/vms/{id}/resume` | none | `OpTiming` | resume the vCPU |
| POST | `/vms/{id}/snapshot` | `SnapshotRequest` | `SnapshotResponse` | full or diff snapshot |
| POST | `/vms/{id}/push` | `PushRequest` | `PushResponse` | stream local files to a peer hostd |
| POST | `/vms/{id}/load` | `LoadRequest` | `OpTiming` | load a snapshot into a prepared VM |
| POST | `/vms/{id}/cutover` | `CutoverRequest` | `CutoverResponse` | pause, final diff, push, remote load; source side only |
| POST | `/files/base?dir=&name=` | octet-stream | `FileWriteResponse` | receive one whole file |
| POST | `/files/extents?dir=&name=` | extent stream | `FileWriteResponse` | apply sparse extents to a file in place |

Extent wire format (`/files/extents` body, and the sparse half of
`/push`): repeated frames of `[offset uint64 LE][length uint64 LE]
[length bytes of data]` until EOF. The receiver `pwrite`s each frame
at its offset into the named file.

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
and `--target`: it boots vm0 itself (step 1), prepares the target,
runs the pre-copy rounds, executes cutover, and on success prints one
final greppable line:

```
RESULT blackout_ms=<float> extents=<int> diff_bytes=<int> max_gap_ms=<float> pass=<true|false>
```

`max_gap_ms` comes from `migrate` querying the observer itself;
`pass` folds both the source-clock and observer numbers against the
30ms budget. Process exit code is nonzero when `pass` is false.

## Measuring blackout

Two independent numbers, both printed by `migratectl migrate` and
both checked by `scripts/demo.sh`:

1. **Source-clock cutover timing.** `CutoverResponse.BlackoutMs`,
   timed on host-a between the pause call returning and the target's
   `/load` acknowledging the guest resumed. This is what the
   orchestrator itself believes happened, and it is the more
   optimistic of the two numbers, since it never leaves host-a's
   clock.
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
- **Docker containers as "hosts".** Two full VMs would also work but
  would need nested nested-virtualization on the Lima VM, which is
  not reliably supported. Two privileged containers sharing the Lima
  VM's kernel and `/dev/kvm` get the same process and network
  isolation that matters for this exercise (each host is its own
  Firecracker process, own tap, own IP) without a second layer of
  virtualization.
- **Firecracker pinned to v1.14.4.** Both containers run the exact
  same build from the same release tarball. Snapshot compatibility
  across Firecracker versions is not guaranteed, and version drift
  between source and target would be an easy way to fail migration
  for reasons that have nothing to do with the algorithm.
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

## Limitations

- Single guest, single migration path, host-a to host-b. No back and
  forth, no concurrent migrations.
- No authentication or TLS on the hostd API. It listens only inside
  the Docker network, which is the intended trust boundary here.
- No retry or rollback if cutover fails partway. The guest is left
  paused on the source and the operator has to intervene.
- The beacon is a synthetic liveness signal, a UDP counter, not a real
  application. It proves the guest was frozen for X ms and nothing
  else. It says nothing about, for example, how a stateful TCP server
  would behave mid-request.
- Diff round count and size threshold are fixed defaults, not tuned
  per workload. A guest that writes memory faster than this one would
  need different tuning to keep the final diff small.
</content>
<parameter name="i">Rewrite README without em-dashes