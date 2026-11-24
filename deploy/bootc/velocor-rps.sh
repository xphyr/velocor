#!/bin/bash
# velocor-rps IFACE: spreads a NIC's receive processing over every CPU (RPS, with RFS) when it
# has fewer receive queues than there are CPUs. Run by udev as NICs appear
# (60-velocor-rps.rules).
#
# The kernel targets do their network work on the CPU that received the connection's packets:
# nvmet-tcp queues each connection's I/O work there. On a NIC with one receive queue (a virtio-net
# NIC without multiqueue, as on the test appliance) every connection then lands on one CPU: 4k
# random reads over NVMe/TCP went from 31.9k to 47.1k IOPS on 4 vCPUs with this. A NIC with a
# queue per CPU (RSS) spreads flows itself and is left alone.
set -eu
dev=/sys/class/net/${1:?interface}
[ -e "$dev/device" ] || exit 0 # virtual: loopback, bridges, bonds' own queues
cpus=$(nproc --all)
queues=("$dev"/queues/rx-*)
[ -e "${queues[0]}" ] && [ "${#queues[@]}" -lt "$cpus" ] || exit 0

# All CPUs as rps_cpus wants it: hex, 32-bit groups separated by commas, most significant first.
mask=""
for ((i = (cpus + 31) / 32 - 1; i >= 0; i--)); do
	bits=$((cpus - i * 32 < 32 ? cpus - i * 32 : 32))
	mask+=$(printf '%08x' $(((1 << bits) - 1)))
	if ((i > 0)); then mask+=,; fi
done
# RFS: each queue gets its share of the global flow table, net.core.rps_sock_flow_entries
# (32768, 90-velocor.conf: not read here, as udev may get here before it is applied).
flows=$((32768 / ${#queues[@]}))
for q in "${queues[@]}"; do
	echo "$mask" >"$q/rps_cpus"
	echo "$flows" >"$q/rps_flow_cnt"
done
