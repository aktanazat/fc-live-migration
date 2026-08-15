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

## What the human did

The project owner (not the AI) made every architecture and environment
decision before any code was written: Docker containers as the
migration hosts, a Lima VM for nested virtualization on Apple Silicon,
the specific pre-copy-plus-cutover algorithm and where the blackout
boundary falls, the choice to measure blackout two ways (source clock
and an independent UDP observer) rather than trust a single
self-reported number, and the decision to pin Firecracker to a single
version on both sides. The owner also wrote the frozen contract that
every agent slice worked against, reviewed the resulting code, and
ran the actual verification: booting the Lima VM, building and running
the containers, and confirming the migration and its blackout
measurement on real hardware.

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
</content>
<parameter name="i">Write AI usage disclosure