#!/bin/sh
# Writes one order into the primary database every half second, until it is
# ended, so that the replica has changes to apply. A scenario runs it as the
# transient unit orders-written-every-half-second.service.
while :; do
  mariadb --execute 'INSERT INTO shop.orders (amount) VALUES (9.99)'
  sleep 0.5
done
