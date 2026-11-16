#!/bin/sh
# Opens COUNT connections to the primary database through its local socket
# and keeps each one open and idle until it is ended: each client waits for
# a statement that never comes. A scenario runs it as the transient unit
# database-connections-held-open.service.
#   sh step_commands/hold_database_connections.sh COUNT
for connection in $(seq "$1"); do
  sleep 3600 | mariadb &
done
wait
