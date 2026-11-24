#!/bin/bash
set -euo pipefail

HOSTNAME_VAL="$(hostname -s 2>/dev/null || hostname 2>/dev/null || echo "velocor")"

# Determine primary outbound IPv4 + interface, falling back to first global IPv4.
PRIMARY_IP=""
PRIMARY_IF=""
if route_out="$(ip -4 route get 1.1.1.1 2>/dev/null)"; then
  PRIMARY_IP="$(awk '{for(i=1;i<=NF;i++) if($i=="src"){print $(i+1); exit}}' <<<"$route_out")"
  PRIMARY_IF="$(awk '{for(i=1;i<=NF;i++) if($i=="dev"){print $(i+1); exit}}' <<<"$route_out")"
fi

ALL_IPS=""
while read -r _ ifname _ cidr _; do
  ip_only="${cidr%%/*}"
  if [ -z "$PRIMARY_IP" ]; then
    PRIMARY_IP="$ip_only"
    PRIMARY_IF="$ifname"
  fi
  entry="${ip_only} (${ifname})"
  if [ -z "$ALL_IPS" ]; then
    ALL_IPS="$entry"
  else
    ALL_IPS="${ALL_IPS}, ${entry}"
  fi
done < <(ip -4 -o addr show scope global 2>/dev/null | grep -v -E '^\S+\s+(lo|veth|podman|cni-|docker)' || true)

VELOD_STATE="$(systemctl is-active velocor-velod.service 2>/dev/null || echo "inactive")"

if [ -n "$PRIMARY_IP" ]; then
  PRIMARY_LINE="${PRIMARY_IP} (${PRIMARY_IF})"
  WEB_UI_URL="https://${PRIMARY_IP}:8443"
else
  PRIMARY_LINE="No IPv4 address assigned (check link / DHCP)"
  WEB_UI_URL="https://<appliance-ip>:8443"
  ALL_IPS="--"
fi

TMP_ISSUE="$(mktemp /run/velocor-issue.XXXXXX)"
cat >"$TMP_ISSUE" <<EOF

================================================================================
  VELOCOR // All-Flash Storage Appliance (\S)
================================================================================
  Hostname   : ${HOSTNAME_VAL}
  Primary IP : ${PRIMARY_LINE}
  All IPs    : ${ALL_IPS}

  Web UI     : ${WEB_UI_URL}
  CLI Login  : veloctl -endpoint ${WEB_UI_URL} login

  Services   : velod=${VELOD_STATE}
  Console    : \l   (\t)
================================================================================

EOF

chmod 0644 "$TMP_ISSUE"
mv -f "$TMP_ISSUE" /etc/issue

# Redraw idle login prompts so the updated IP appears immediately on the console.
for tty_unit in getty@tty1.service serial-getty@ttyS0.service; do
  tty_dev="${tty_unit#*@}"
  tty_dev="${tty_dev%.service}"
  if systemctl is-active --quiet "$tty_unit" 2>/dev/null; then
    if ! who 2>/dev/null | grep -qw "$tty_dev"; then
      systemctl restart "$tty_unit" 2>/dev/null || true
    fi
  fi
done
