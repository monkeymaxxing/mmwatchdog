#!/bin/sh
# The caches, the local DNS cache, and the mail server of the application
# server (mmwatchdog-dev):
#   - two instances of Redis from the package's template unit:
#     redis-server@sessions.service on port 6380 for the sessions, and
#     redis-server@job-queue.service on port 6381 for the job queue. The
#     package's own redis-server.service on 6379 is not used;
#   - memcached on 127.0.0.1:11211, with Restart=no: MMWatchdog only reports it;
#   - dnsmasq, the local DNS cache, on 127.0.0.1:53 beside systemd-resolved,
#     which answers app.example.com with 192.0.2.10 and is not the
#     machine's resolver;
#   - postfix, which accepts mail on port 25 of every address, so that
#     mmwatchdog-peer can check it.
set -eu
systemctl disable --now redis-server.service
for instance in sessions:6380 job-queue:6381; do
  name=${instance%%:*}
  port=${instance#*:}
  cat > "/etc/redis/redis-$name.conf" <<REDIS
# Redis for the $name of the web application, as redis-server@$name.service.
port $port
bind 127.0.0.1
protected-mode yes
pidfile /run/redis-$name/redis-server.pid
logfile /var/log/redis/redis-server-$name.log
dir /var/lib/redis
dbfilename dump-$name.rdb
save ""
REDIS
  chown redis:redis "/etc/redis/redis-$name.conf"
  chmod 0640 "/etc/redis/redis-$name.conf"
  systemctl enable --now "redis-server@$name.service"
  for attempt in $(seq 50); do
    redis-cli -p "$port" PING 2>/dev/null | grep -qx PONG && break
    sleep 0.2
  done
  redis-cli -p "$port" PING | grep -qx PONG
done

install -d -m 0755 /etc/systemd/system/memcached.service.d
cat > /etc/systemd/system/memcached.service.d/web-service-fleet.conf <<'UNIT'
# A memcached that ends stays down: the example's MMWatchdog reports it and leaves it alone.
[Service]
Restart=no
UNIT

cat > /etc/dnsmasq.d/web-service-fleet.conf <<'DNSMASQ'
# The local DNS cache: it answers on 127.0.0.1 only, beside systemd-resolved
# on 127.0.0.53, and knows one name.
listen-address=127.0.0.1
bind-interfaces
no-resolv
no-hosts
address=/app.example.com/192.0.2.10
DNSMASQ
# The machine keeps systemd-resolved as its resolver: dnsmasq does not
# register itself with resolvconf.
cat >> /etc/default/dnsmasq <<'DEFAULT'
IGNORE_RESOLVCONF=yes
DNSMASQ_EXCEPT="lo"
DEFAULT

postconf -e inet_interfaces=all

systemctl daemon-reload
for unit in memcached.service dnsmasq.service postfix.service; do
  systemctl enable "$unit"
  systemctl restart "$unit"
done
# Each service answers before the setup ends.
answers() {
  for attempt in $(seq 50); do
    python3 -c "$1" 2>/dev/null && return 0
    sleep 0.2
  done
  echo "$2 does not answer" >&2
  return 1
}
answers '
import socket, struct, sys
query = struct.pack(">HHHHHH", 0x5745, 0x0100, 1, 0, 0, 0) + b"\x03app\x07example\x03com\x00" + struct.pack(">HH", 1, 1)
connection = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
connection.settimeout(1)
connection.sendto(query, ("127.0.0.1", 53))
sys.exit(0 if connection.recv(512).endswith(bytes([192, 0, 2, 10])) else 1)' 'dnsmasq on 127.0.0.1:53'
answers '
import socket, sys
connection = socket.create_connection(("127.0.0.1", 11211), 2)
connection.sendall(b"stats\r\n")
sys.exit(0 if b"STAT pid" in connection.recv(65536) else 1)' 'memcached on 127.0.0.1:11211'
answers '
import socket, sys
connection = socket.create_connection(("127.0.0.1", 25), 5)
sys.exit(0 if connection.recv(512).startswith(b"220") else 1)' 'postfix on port 25'
