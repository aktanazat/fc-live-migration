# fc-live-migration

Moves a running Firecracker microVM from one host to another while it
keeps running, with a blackout under 30 ms. Firecracker has snapshots
and dirty-page tracking but no live migration; this builds pre-copy
live migration on top of them.

Written in August 2026 as the Dedalus Labs take-home, which set the
30 ms budget. Demo video (83 s, no audio):
https://youtu.be/UnGWq82bNTs

## What it does

- `hostd` runs on each host and puts a small REST API in front of the
  Firecracker process it manages.
- `migratectl` drives two `hostd`s. It ships the guest's memory to the
  target while the guest keeps running, then sends only the pages
  dirtied since, in rounds, until a round is 1 MiB or less (at most 8
  rounds). Then it pauses the guest, sends the last dirty pages, and
  resumes it on the target.
- Memory moves host to host on its own network. The orchestrator only
  sends control calls.
- The guest keeps its IP and MAC, so the observer below sees a gap in
  packets, not a lost connection.

The two hosts are Docker containers inside one Lima VM on an Apple
Silicon Mac, each running Firecracker on the Lima VM's `/dev/kvm`. The
snapshots and the network hop between hosts are real; the two
physical machines are not.

Design, API, and the bugs found during integration:
[docs/design.md](docs/design.md). How AI was used: [AI.md](AI.md).

## Measured blackout

Blackout is measured two ways, and a run passes only if both are at or
under 30 ms:

1. Source clock. `migratectl` adds up every moment the guest was
   paused: the brief pauses for the snapshots taken before cutover,
   plus cutover itself (pause on the source until the target confirms
   the guest resumed).
2. Independent observer. A program inside the guest sends a UDP
   packet every 2 ms to a separate container that plays no part in the
   migration. The observer reports the longest gap between arrivals.

Single runs from `make demo` on 2026-08-15, final code, the run shown
in the video. Guest: 1 vCPU, 128 MiB. Lima VM: 8 vCPUs, 10 GiB.

| direction | source-clock blackout | observer max gap |
|---|---|---|
| host-a → host-b | 11.8 ms | 3.9 ms |
| host-b → host-a (same guest, back) | 12.8 ms | 8.3 ms |

In the return leg, cutover took 6.9 ms (pause 0.4, final snapshot 1.7,
push 2.2, load 2.6); the other 5.9 ms were two snapshot pauses before
cutover. An earlier run, before the background checkpointer was added,
measured 18.6 ms (observer 22.3 ms) one way and 9.9 ms (observer
2.9 ms) back.

## Run it

Needs an M3-or-newer Mac on macOS 15+ (for nested virtualization),
[Lima](https://lima-vm.io), and about 10 GiB of free memory for the
Lima VM.

```sh
limactl start lima/fcdev.yaml
limactl shell fcdev
```

Inside the Lima VM, from the repo root (it is mounted at the same
path as on the Mac):

```sh
make kernel    # fetch a prebuilt aarch64 vmlinux into artifacts/
make rootfs    # build a minimal ext4 rootfs containing the beacon
make build     # build hostd, observer and migratectl
make images    # build the host container image
make up        # start host-a, host-b and the observer
make demo      # migrate host-a -> host-b and back, check both against 30 ms
```

`make migrate` runs a single migration. Each migration ends with one
line such as:

```
RESULT total_blackout_ms=12.791 cutover_blackout_ms=6.880 extents=24 diff_bytes=552960 max_gap_ms=8.300 pass=true
```

`make down` stops the containers; `make clean` removes `artifacts/`
and `bin/`.

## License

MIT, see [LICENSE](LICENSE).
