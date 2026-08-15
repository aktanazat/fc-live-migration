# AI usage

This project was built with agentic AI coding tools, used deliberately
and disclosed here in specific terms, as the challenge's AI policy
asks.

## What the AI did

The implementation was written by parallel Claude-based coding agents
(Anthropic's Claude Code / Claude Agent SDK), running concurrently
against a single frozen API contract (`internal/api/types.go` plus a
written cross-slice contract document covering topology, the
migration algorithm, file layout, and Makefile targets). Each agent
owned a disjoint slice of the repository:

- one slice: `hostd` (the per-host REST server, Firecracker API socket
  client, sparse-file extent walking and application)
- one slice: `migratectl` (the orchestrator: pre-copy loop, cutover
  driver, timing report)
- one slice: the guest beacon, the observer, and the Docker/Firecracker
  environment (rootfs build, kernel fetch, container image, entrypoint,
  compose file)
- one slice: this documentation, the Makefile, and the demo script

A separate read-only research agent was used specifically to verify
Firecracker v1.14.4 snapshot API semantics (the exact request bodies
for `PUT /snapshot/create` and `PUT /snapshot/load`, dirty-page-
tracking behavior across repeated diff snapshots, and the aarch64
developer-preview status of diff snapshots) against Firecracker's own
upstream documentation before that behavior was assumed anywhere in
the contract. This was a deliberate check against model knowledge
being stale or wrong about a fast-moving external API, not a
substitute for reading the docs.

## Integration debugging

Once the slices were merged and run against real Firecracker
processes, a migration was found to complete cleanly and still leave
the target guest running on stale memory in some address range. This
was agent-driven root-causing under human direction, not a bug found
and fixed by a human alone: an agent byte-compared a full snapshot of
the guest against the local base a diff round claimed to have already
merged, found the mismatch, then instrumented both ends of the push
path with a hash of the mem file at send and at receive to bisect
whether the loss happened in the merge, in the transfer, or before
either. That traced it to a diff snapshot taken and then not pushed
on an error path: Firecracker had already reset its dirty-page bitmap
for that snapshot, so the pages it covered were unrecoverable once
the push was skipped, gone from the old base and untracked by the
bitmap alike. The owner directed the investigation (byte-compare
first, then wire hashing, not the reverse) and signed off on the fix,
which the agent implemented: the pre-copy loop's convergence check
now runs only after a diff's push succeeds, and cutover's failure
path folds the final diff into the local base before returning an
error. See the README's Pitfalls section for the user-facing version
of this.

## What the human did

The project owner (not the AI) made every architecture and environment
decision before any code was written: Docker containers as the
migration hosts, a Lima VM for nested virtualization on Apple Silicon,
the specific pre-copy-plus-cutover algorithm and where the blackout
boundary falls, the choice to measure blackout two ways (source clock
and an independent UDP observer) rather than trust a single
self-reported number, the decision to pin Firecracker to a single
version on both sides, and the addition of a dedicated migration
network once beacon packets were observed queuing behind snapshot
pushes on a shared bridge. The owner also wrote the frozen contract
that every agent slice worked against, directed the integration
debugging above, reviewed the resulting code, and ran the actual
verification: booting the Lima VM, building and running the
containers, and confirming the migration and its blackout measurement
on real hardware in both directions.

## Why this split

Splitting the work by API boundary (hostd's REST surface, the
migratectl client of that surface, the guest/container environment,
and the docs) let independent agents build each side without stepping
on each other, as long as the contract between them did not move.
That contract was fixed by the human up front and treated as
authoritative; any slice that needed to change it had to say so
explicitly rather than silently drifting the wire format. This is the
same reason the contract exists as a written document instead of being
implicit in one agent's memory: nothing in this project crosses a
process boundary without a type that both sides agreed on before
either side wrote code against it.
