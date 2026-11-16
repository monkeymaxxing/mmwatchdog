#!/bin/sh
# Drops, or lets through again, one kind of traffic that comes to this guest
# from the other guest: new SSH connections (ssh), or ping requests (ping).
# The rules live in the nftables table inet web_service_fleet_test_blocks.
# The test harness reaches the guests from another address, so its own SSH
# connections go on.
#   sh step_commands/traffic_from_the_other_guest.sh block|allow ssh|ping OTHER_GUEST_HOST_NAME
set -eu
action=$1
traffic=$2
other_guest=$(getent hosts "$3" | awk '{print $1}')
[ -n "$other_guest" ]
table='inet web_service_fleet_test_blocks'
case $traffic in
  ssh) match='tcp dport 22' ;;
  ping) match='icmp type echo-request' ;;
  *) echo "unknown traffic $traffic: ssh or ping" >&2; exit 2 ;;
esac
nft add table $table
nft "add chain $table input { type filter hook input priority 0; policy accept; }"
case $action in
  block) nft add rule $table input ip saddr "$other_guest" $match drop comment "\"$traffic\"" ;;
  allow)
    for handle in $(nft --handle list chain $table input | awk -v traffic="\"$traffic\"" '$0 ~ "comment " traffic {print $NF}'); do
      nft delete rule $table input handle "$handle"
    done ;;
  *) echo "unknown action $action: block or allow" >&2; exit 2 ;;
esac
nft list chain $table input
