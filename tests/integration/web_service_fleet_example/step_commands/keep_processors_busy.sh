#!/bin/sh
# Keeps every processor of the machine busy until it is ended: one endless
# loop for each processor. A scenario runs it at the lowest priority as the
# transient unit processors-kept-busy.service, so that MMWatchdog still runs.
for processor in $(seq "$(nproc)"); do
  (while :; do :; done) &
done
wait
