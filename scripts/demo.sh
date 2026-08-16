#!/usr/bin/env bash
# End-to-end demo, meant to run start to finish in well under two
# minutes for screen recording. Assumes `make up` has already brought
# host-a, host-b, and observer up.
#
# `migratectl migrate` boots vm0 on the source itself (contract step 1
# of the migration algorithm), so this script does not pre-boot vm0:
# doing so would make hostd reject migrate's own boot as a duplicate.
# Instead it smoke-tests the environment with a throwaway VM id first
# (boot, confirm the beacon reaches the observer, tear down), then
# runs the real migration of vm0.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

HOSTD_A="http://127.0.0.1:8081"
HOSTD_B="http://127.0.0.1:8082"
OBSERVER="http://127.0.0.1:9090"
PEER_A="http://172.31.0.11:8080"
SMOKE_ID="smoke0"
BUDGET_MS="30"
MIGRATECTL="bin/migratectl"
BEACON_WAIT_ITERS="60"
BEACON_WAIT_SLEEP="0.25"

step() {
	printf '\n[%s] %d/6  %s\n' "$(date +%H:%M:%S)" "$1" "$2"
}

# Polls the observer's packet count until it is nonzero. Prints the
# count and returns 0 on success; returns 1 after BEACON_WAIT_ITERS
# tries with no packets.
wait_for_beacon() {
	local packets
	for _ in $(seq 1 "$BEACON_WAIT_ITERS"); do
		packets="$(curl -fsS "$OBSERVER/report" | jq -r '.packets')"
		if [[ "$packets" -gt 0 ]]; then
			echo "$packets"
			return 0
		fi
		sleep "$BEACON_WAIT_SLEEP"
	done
	return 1
}

if [[ ! -x "$MIGRATECTL" ]]; then
	echo "building bin/migratectl"
	make build
fi

step 1 "resetting observer stats"
curl -fsS -X POST "$OBSERVER/reset" >/dev/null
echo "observer stats cleared"

step 2 "smoke-testing host-a: boot, confirm beacon, tear down"
"$MIGRATECTL" boot --host "$HOSTD_A" --vm "$SMOKE_ID" \
	--kernel /artifacts/vmlinux --rootfs /artifacts/rootfs.ext4 \
	--vcpus 1 --mem-mib 128
if packets="$(wait_for_beacon)"; then
	echo "beacon alive: $packets packets received from $SMOKE_ID"
else
	echo "FAIL: no beacon packets observed, $SMOKE_ID never came up" >&2
	exit 1
fi
curl -fsS -X DELETE "$HOSTD_A/vms/$SMOKE_ID" >/dev/null
curl -fsS -X POST "$OBSERVER/reset" >/dev/null
echo "$SMOKE_ID torn down, observer reset for the real run"

step 3 "migrating vm0 host-a -> host-b (migrate boots, pre-copies, cuts over)"
# --settle-packets 2000 keeps the guest running ~4s before the measured
# window so the background checkpointer's first tick absorbs kernel-boot
# dirt into the base; the migration itself then moves only steady-state
# dirt.
migrate_out="$("$MIGRATECTL" migrate --source "$HOSTD_A" --target "$HOSTD_B" --settle-packets 2000 2>&1 | tee /dev/stderr)"

result_line="$(printf '%s\n' "$migrate_out" | grep '^RESULT ' | tail -n1)"
if [[ -z "$result_line" ]]; then
	echo "FAIL: no RESULT line in migratectl output" >&2
	exit 1
fi
blackout_ms="$(printf '%s' "$result_line" | sed -n 's/.*total_blackout_ms=\([0-9.]*\).*/\1/p')"
result_max_gap_ms="$(printf '%s' "$result_line" | sed -n 's/.*max_gap_ms=\(-\?[0-9.]*\).*/\1/p')"
result_pass="$(printf '%s' "$result_line" | sed -n 's/.*pass=\(true\|false\).*/\1/p')"

step 4 "reading the independent observer report"
report="$(curl -fsS "$OBSERVER/report")"
echo "$report" | jq .
observer_max_gap_ms="$(printf '%s' "$report" | jq -r '.max_gap_ms')"

step 5 "verdict"
printf 'blackout_ms=%s migratectl_max_gap_ms=%s observer_max_gap_ms=%s budget_ms=%s\n' \
	"$blackout_ms" "$result_max_gap_ms" "$observer_max_gap_ms" "$BUDGET_MS"

pass=1
[[ "$result_pass" == "true" ]] || pass=0
awk -v g="$observer_max_gap_ms" -v budget="$BUDGET_MS" 'BEGIN { exit !(g <= budget) }' || pass=0

if [[ "$pass" -eq 1 ]]; then
	echo "PASS: vm0 migrated host-a -> host-b within the ${BUDGET_MS}ms blackout budget"
else
	echo "FAIL: blackout exceeded ${BUDGET_MS}ms (migratectl verdict or observer gap)"
	exit 1
fi

step 6 "migrating the same running guest back host-b -> host-a"
reverse_out="$("$MIGRATECTL" migrate --source "$HOSTD_B" --target "$HOSTD_A" --peer "$PEER_A" 2>&1 | tee /dev/stderr)"
reverse_line="$(printf '%s\n' "$reverse_out" | grep '^RESULT ' | tail -n1)"
reverse_pass="$(printf '%s' "$reverse_line" | sed -n 's/.*pass=\(true\|false\).*/\1/p')"
if [[ "$reverse_pass" == "true" ]]; then
	echo "PASS: round trip complete; the guest never stopped serving"
else
	echo "FAIL: reverse migration exceeded the blackout budget" >&2
	exit 1
fi
