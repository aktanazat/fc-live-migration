#!/usr/bin/env bash
# Fetches the newest Firecracker CI aarch64 5.10 kernel (vmlinux) into
# artifacts/vmlinux. Idempotent: skips the network round trip entirely
# if the file already exists.
set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
artifacts_dir="${repo_root}/artifacts"
dest="${artifacts_dir}/vmlinux"

if [[ -f "${dest}" ]]; then
	echo "fetch-kernel: ${dest} already present, skipping" >&2
	exit 0
fi

mkdir -p "${artifacts_dir}"

# S3 ListObjectsV2 over the public Firecracker CI bucket, restricted to
# the 5.10 aarch64 kernels for our pinned firecracker-ci prefix (see
# the Firecracker "Getting Started" guide's kernel-listing pattern).
listing_url="http://spec.ccfc.min.s3.amazonaws.com/?prefix=firecracker-ci/v1.14/aarch64/vmlinux-5.10&list-type=2"

echo "fetch-kernel: listing kernels at ${listing_url}" >&2
listing="$(curl -fsSL "${listing_url}")"

# XML <Key>...</Key> entries name every matching object; the highest
# version-sorted key is the newest kernel build.
key="$(grep -o '<Key>[^<]*</Key>' <<<"${listing}" | sed -e 's#<Key>##' -e 's#</Key>##' | sort -V | tail -1)"

if [[ -z "${key}" ]]; then
	echo "fetch-kernel: no matching vmlinux-5.10 key found in bucket listing" >&2
	exit 1
fi

download_url="https://s3.amazonaws.com/spec.ccfc.min/${key}"
echo "fetch-kernel: downloading ${download_url} -> ${dest}" >&2
curl -fsSL -o "${dest}.tmp" "${download_url}"
mv "${dest}.tmp" "${dest}"

echo "fetch-kernel: wrote ${dest}" >&2
