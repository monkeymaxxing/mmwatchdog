#!/usr/bin/env python3
"""Takes memory and holds it until the program is ended: MEBIBYTES MiB, or
as much as makes the machine's memory used to PERCENT percent, as MMWatchdog's
node_resource memory measures it (100 x (1 - MemAvailable / MemTotal)).
A scenario runs it as a transient systemd unit and stops the unit again.

  python3 step_commands/hold_memory.py --mebibytes MEBIBYTES
  python3 step_commands/hold_memory.py --until-memory-used-percent PERCENT
"""
import argparse
from pathlib import Path
import time


def memory_used_percent():
    fields = {line.split(':')[0]: int(line.split()[1]) for line in Path('/proc/meminfo').read_text().splitlines()}
    return 100 * (1 - fields['MemAvailable'] / fields['MemTotal'])


parser = argparse.ArgumentParser()
amount = parser.add_mutually_exclusive_group(required=True)
amount.add_argument('--mebibytes', type=int)
amount.add_argument('--until-memory-used-percent', type=float)
arguments = parser.parse_args()
held = []
# 16 MiB at a time, each byte written, so that the memory is in use.
if arguments.mebibytes is not None:
    for _ in range(arguments.mebibytes // 16):
        held.append(bytearray(b'\x01') * (16 * 1024 * 1024))
else:
    while memory_used_percent() < arguments.until_memory_used_percent:
        held.append(bytearray(b'\x01') * (16 * 1024 * 1024))
print(f'holding {16 * len(held)} MiB; memory_used_percent is {memory_used_percent():.1f}', flush=True)
while True:
    time.sleep(3600)
