#!/bin/sh
# The databases of the application server (mmwatchdog-dev):
#   - the primary: the package's mariadb.service on 127.0.0.1:3306, with a
#     binary log and at most 20 connections;
#   - its replica: mariadb-replica.service on 127.0.0.1:3307, a second
#     MariaDB server of its own data directory, which follows the primary
#     with GTID;
#   - the accounts: replication for the replica; mmwatchdog_monitoring@127.0.0.1,
#     which may only log in and ask for the status of a replica, for MMWatchdog's
#     mysql checks of the replica; and mmwatchdog@localhost, which logs in through
#     the local socket as the operating system account mmwatchdog and may read the
#     database reporting, for the shell check of the reporting query;
#   - the databases reporting (table daily_totals) and shop (table orders).
set -eu
# The binary log and the replica's error log; Ubuntu's package logs to the journal and makes no such directory.
install -d -o mysql -g adm -m 2750 /var/log/mysql
cat > /etc/mysql/mariadb.conf.d/60-web-service-fleet.cnf <<'CNF'
# The primary database of the web service.
[mariadbd]
bind-address = 127.0.0.1
server_id = 1
log_bin = /var/log/mysql/mariadb-bin
binlog_format = ROW
max_connections = 20
innodb_buffer_pool_size = 32M
CNF
systemctl enable mariadb.service
systemctl restart mariadb.service

cat > /etc/mysql/mariadb-replica.cnf <<'CNF'
# The replica of the web service's primary database, read by
# mariadb-replica.service only.
[mariadbd]
user = mysql
datadir = /var/lib/mysql-replica
socket = /run/mysqld-replica/mysqld.sock
pid-file = /run/mysqld-replica/mysqld.pid
port = 3307
bind-address = 127.0.0.1
server_id = 2
relay_log = /var/lib/mysql-replica/replica-relay-bin
log_error = /var/log/mysql/replica-error.log
innodb_buffer_pool_size = 32M
CNF
if [ ! -d /var/lib/mysql-replica/mysql ]; then
  mariadb-install-db --defaults-file=/etc/mysql/mariadb-replica.cnf --user=mysql >/dev/null
fi
cat > /etc/systemd/system/mariadb-replica.service <<'UNIT'
[Unit]
Description=The replica of the web service's MariaDB database, on 127.0.0.1:3307
After=network.target mariadb.service
[Service]
Type=notify
User=mysql
Group=mysql
RuntimeDirectory=mysqld-replica
ExecStart=/usr/sbin/mariadbd --defaults-file=/etc/mysql/mariadb-replica.cnf
TimeoutStartSec=120
Restart=no
[Install]
WantedBy=multi-user.target
UNIT
systemctl daemon-reload
systemctl enable --now mariadb-replica.service

replication_password=$(openssl rand -hex 16)
monitoring_password=$(openssl rand -hex 16)
install -d -m 0700 /etc/mmwatchdog /etc/mmwatchdog/secrets
chmod 0755 /etc/mmwatchdog
printf '%s\n' "$monitoring_password" > /etc/mmwatchdog/secrets/mariadb-replica-mmwatchdog-monitoring-password
chmod 0600 /etc/mmwatchdog/secrets/mariadb-replica-mmwatchdog-monitoring-password

mariadb <<SQL
CREATE USER IF NOT EXISTS 'replication'@'127.0.0.1' IDENTIFIED BY '$replication_password';
GRANT REPLICATION SLAVE ON *.* TO 'replication'@'127.0.0.1';
CREATE USER IF NOT EXISTS 'mmwatchdog_monitoring'@'127.0.0.1' IDENTIFIED BY '$monitoring_password';
GRANT REPLICA MONITOR ON *.* TO 'mmwatchdog_monitoring'@'127.0.0.1';
CREATE USER IF NOT EXISTS 'mmwatchdog'@'localhost' IDENTIFIED VIA unix_socket;
CREATE DATABASE IF NOT EXISTS reporting;
CREATE TABLE IF NOT EXISTS reporting.daily_totals (day DATE PRIMARY KEY, orders INT NOT NULL);
INSERT IGNORE INTO reporting.daily_totals VALUES ('2026-10-01', 412), ('2026-10-02', 397);
GRANT SELECT ON reporting.* TO 'mmwatchdog'@'localhost';
CREATE DATABASE IF NOT EXISTS shop;
CREATE TABLE IF NOT EXISTS shop.orders (id INT AUTO_INCREMENT PRIMARY KEY, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, amount DECIMAL(10,2) NOT NULL);
SQL

# The replica starts from the beginning of the primary's binary log, so it
# applies every statement above. Its anonymous accounts would take the
# logins of mmwatchdog_monitoring from 127.0.0.1.
mariadb --socket=/run/mysqld-replica/mysqld.sock <<SQL
DROP USER IF EXISTS ''@'localhost';
DROP USER IF EXISTS ''@'$(hostname)';
STOP SLAVE;
CHANGE MASTER TO MASTER_HOST='127.0.0.1', MASTER_PORT=3306, MASTER_USER='replication',
  MASTER_PASSWORD='$replication_password', MASTER_USE_GTID=slave_pos, MASTER_DELAY=0;
START SLAVE;
SQL
for attempt in $(seq 100); do
  rows=$(mariadb --socket=/run/mysqld-replica/mysqld.sock --batch --skip-column-names \
    --execute 'SELECT COUNT(*) FROM reporting.daily_totals' 2>/dev/null || true)
  [ "$rows" = 2 ] && break
  sleep 0.2
done
[ "$rows" = 2 ]
mariadb --socket=/run/mysqld-replica/mysqld.sock --execute 'SHOW SLAVE STATUS\G' | grep -E 'Slave_(IO|SQL)_Running:|Seconds_Behind_Master:'
# The monitoring account logs in to the replica over TCP.
MYSQL_PWD=$monitoring_password mariadb --protocol=TCP --host=127.0.0.1 --port=3307 --user=mmwatchdog_monitoring --skip-ssl \
  --execute 'SHOW SLAVE STATUS\G' | grep -q 'Slave_SQL_Running: Yes'
