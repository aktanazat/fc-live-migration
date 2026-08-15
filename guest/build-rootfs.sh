#!/usr/bin/env bash
# Builds artifacts/rootfs.ext4: a minimal ext4 image containing only
# /init, the statically linked beacon binary that runs as guest PID 1.
# Built entirely from a staging directory via `mke2fs -d`, so no sudo
# or loop-device mount is required.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
artifacts_dir="${repo_root}/artifacts"
dest="${artifacts_dir}/rootfs.ext4"
beacon_dir="${repo_root}/guest/beacon"

if ! command -v go >/dev/null 2>&1; then
	echo "build-rootfs: 'go' not found on PATH. Run this inside the Lima VM" \
		"(see lima/fcdev.yaml) where Go 1.23+ is provisioned." >&2
	exit 1
fi

if ! command -v mke2fs >/dev/null 2>&1; then
	echo "build-rootfs: 'mke2fs' (e2fsprogs) not found on PATH. Run this" \
		"inside the Lima VM (see lima/fcdev.yaml), which ships e2fsprogs." >&2
	exit 1
fi

mkdir -p "${artifacts_dir}"

staging_dir="$(mktemp -d)"
trap 'rm -rf "${staging_dir}"' EXIT

echo "build-rootfs: building beacon -> ${staging_dir}/init" >&2
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o "${staging_dir}/init" "${beacon_dir}"

rm -f "${dest}"
echo "build-rootfs: writing ${dest}" >&2
mke2fs -t ext4 -d "${staging_dir}" "${dest}" 64m

echo "build-rootfs: wrote ${dest}" >&2
