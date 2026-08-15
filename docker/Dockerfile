# syntax=docker/dockerfile:1
#
# Image for host-a/host-b/observer (fc-host). Everything is aarch64:
# the Lima VM, its containers, and Firecracker itself.
#
# Build context is the repo root (see Makefile's `images` target and
# docker/compose.yaml's `build.context: ..`), so all COPY sources below
# are repo-root-relative.

ARG FC_VERSION=v1.14.4
# FC_SOURCE_BUILD selects how the firecracker binary is produced:
#   0 (default) - fetch the pinned upstream release tarball and untar it.
#   1            - git clone FC_VERSION and `cargo build --release` it.
ARG FC_SOURCE_BUILD=0

# ---------------------------------------------------------------------------
# Stage: go-build — compiles hostd and observer as static aarch64 binaries.
# ---------------------------------------------------------------------------
FROM golang:1.23 AS go-build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /out/hostd ./cmd/hostd
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /out/observer ./cmd/observer

# ---------------------------------------------------------------------------
# Stage: fc-source-build — FC_SOURCE_BUILD=1 path: build Firecracker from
# source at FC_VERSION via cargo, natively for aarch64 (this stage's base
# image is itself aarch64 when built on an aarch64 host, matching the
# glibc runtime image below — no cross-compilation or musl target needed).
# ---------------------------------------------------------------------------
FROM rust:1-bookworm AS fc-source-build
ARG FC_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
		git \
	&& rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 --branch "${FC_VERSION}" \
		https://github.com/firecracker-microvm/firecracker /src
WORKDIR /src
# Firecracker's own devtool wraps this in a container; unnecessary here
# since we're already inside one. This is the raw cargo equivalent.
RUN cargo build --release --bin firecracker \
	&& cp target/release/firecracker /firecracker

# ---------------------------------------------------------------------------
# Stage: fc-release-fetch — FC_SOURCE_BUILD=0 (default) path: fetch the
# pinned upstream release tarball and untar it. No Rust toolchain needed.
# ---------------------------------------------------------------------------
FROM debian:bookworm-slim AS fc-release-fetch
ARG FC_VERSION
RUN apt-get update && apt-get install -y --no-install-recommends \
		curl ca-certificates \
	&& rm -rf /var/lib/apt/lists/*
RUN curl -fsSL -o /tmp/firecracker.tgz \
		"https://github.com/firecracker-microvm/firecracker/releases/download/${FC_VERSION}/firecracker-${FC_VERSION}-aarch64.tgz" \
	&& tar -xzf /tmp/firecracker.tgz -C /tmp \
	&& cp "/tmp/release-${FC_VERSION}-aarch64/firecracker-${FC_VERSION}-aarch64" /firecracker \
	&& rm -rf /tmp/firecracker.tgz "/tmp/release-${FC_VERSION}-aarch64"

# ---------------------------------------------------------------------------
# Stage: fc-select — picks between the two Firecracker build paths above
# via FC_SOURCE_BUILD. Both upstream stages build unconditionally
# (BuildKit dedupes/caches the unused one cheaply); this stage's only job
# is the ARG-driven `FROM` alias so `runtime` always copies from a single
# fixed stage name regardless of which path produced it.
# ---------------------------------------------------------------------------
FROM fc-release-fetch AS fc-select-0
FROM fc-source-build AS fc-select-1
FROM fc-select-${FC_SOURCE_BUILD} AS fc-select

# ---------------------------------------------------------------------------
# Stage: runtime — final image shipped to host-a/host-b/observer.
# ---------------------------------------------------------------------------
FROM ubuntu:24.04 AS runtime
RUN apt-get update && apt-get install -y --no-install-recommends \
		iproute2 iputils-ping ca-certificates \
		curl \
	&& rm -rf /var/lib/apt/lists/*
# curl above is for docker/compose.yaml's host-a/host-b `/healthz`
# healthchecks; it is not on Firecracker's or hostd's runtime path.

COPY --from=fc-select /firecracker /usr/local/bin/firecracker
COPY --from=go-build /out/hostd /usr/local/bin/hostd
COPY --from=go-build /out/observer /usr/local/bin/observer
COPY docker/entrypoint.sh /entrypoint.sh

RUN chmod +x /usr/local/bin/firecracker /usr/local/bin/hostd /usr/local/bin/observer /entrypoint.sh

ENTRYPOINT ["/entrypoint.sh"]
