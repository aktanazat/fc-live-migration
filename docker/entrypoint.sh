#!/usr/bin/env bash
# Entrypoint for host-a/host-b containers. Rewires container networking
# so the guest's tap device shares a first-class IP on the fcnet Docker
# network with the container itself: eth0 and tap0 are bridged onto
# br0, and the container's original eth0 IP + default route move onto
# br0. This is what lets a migrated guest keep its own IP (172.30.0.50)
# reachable from either host without NAT.
#
# Runs once at container start, then execs hostd as PID 1.
set -eu

# The container sits on two Docker networks: fcnet (guest traffic,
# 172.30.0.0/24) and mignet (bulk migration transfer, 172.31.0.0/24).
# Only the fcnet interface is bridged with the guest tap; interface
# names are assigned by Docker in network-name order, so find the
# fcnet one by its address instead of assuming eth0.
iface_eth0="$(ip -4 -o addr show | awk '$4 ~ /^172\.30\.0\./ {print $2; exit}')"
if [ -z "${iface_eth0}" ]; then
	echo "entrypoint: no interface with a 172.30.0.0/24 address found" >&2
	exit 1
fi
iface_tap0="tap0"
bridge="br0"
# Fallback default gateway if the route capture below (unexpectedly)
# finds nothing; matches fc-contract.md's fcnet topology.
fallback_gateway="172.30.0.1"

# 1. Create the tap device that Firecracker attaches the guest NIC to.
ip tuntap add dev "${iface_tap0}" mode tap

# 2. Create the bridge that will carry both eth0 (the container's
#    Docker network identity) and tap0 (the guest's NIC) as members.
ip link add name "${bridge}" type bridge

# 3. Capture eth0's current IPv4/prefix and default route before
#    touching anything. Docker assigned both; we relocate them onto
#    the bridge rather than reconstruct them from scratch.
eth0_cidr="$(ip -4 -o addr show dev "${iface_eth0}" | awk '{print $4}' | head -n1)"
if [ -z "${eth0_cidr}" ]; then
	echo "entrypoint: no IPv4 address found on ${iface_eth0}" >&2
	exit 1
fi
default_gw="$(ip -4 route show default dev "${iface_eth0}" | awk '{print $3}' | head -n1)"
default_gw="${default_gw:-${fallback_gateway}}"

# 4. Flush eth0's addressing; it becomes a plain bridge port below.
ip addr flush dev "${iface_eth0}"

# 5. Enslave both eth0 and tap0 into the bridge.
ip link set dev "${iface_eth0}" master "${bridge}"
ip link set dev "${iface_tap0}" master "${bridge}"

# 6. Move the captured IP onto the bridge — the bridge now owns the
#    container's identity on fcnet, with eth0 and tap0 as transparent
#    members underneath it.
ip addr add "${eth0_cidr}" dev "${bridge}"

# 7. Bring every interface up: taps and freshly created bridges start
#    administratively down.
ip link set dev "${iface_eth0}" up
ip link set dev "${iface_tap0}" up
ip link set dev "${bridge}" up

# 8. Restore the default route, now via the bridge (flushing eth0 in
#    step 4 dropped it along with the rest of its addressing).
ip route add default via "${default_gw}" dev "${bridge}"

# Optional: some virtio/tap combinations misbehave with checksum/
# segmentation offload once bridged; if guest networking looks
# corrupted (garbled packets, bad checksums), disable offload on tap0:
#   ethtool -K "${iface_tap0}" tx off rx off tso off gso off gro off

# 9. hostd's Firecracker API sockets and PID files live under here.
mkdir -p /run/fc

exec hostd
