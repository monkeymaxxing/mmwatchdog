#!/usr/bin/env python3
"""Succeeds when the machine's memory and swap space are used at least, or
at most, as much as stated, in the percent of MMWatchdog's node_resource:
memory 100 x (1 - MemAvailable / MemTotal), swap 100 x (1 - SwapFree / SwapTotal).

  python3 step_commands/node_memory.py memory|swap at-least|at-most PERCENT
"""
from pathlib import Path
import sys

fields = {line.split(':')[0]: int(line.split()[1]) for line in Path('/proc/meminfo').read_text().splitlines()}
used = {'memory': 100 * (1 - fields['MemAvailable'] / fields['MemTotal']),
        'swap': 100 * (1 - fields['SwapFree'] / fields['SwapTotal']) if fields['SwapTotal'] else 0}
resource, comparison, percent = sys.argv[1], sys.argv[2], float(sys.argv[3])
print(f'{resource} is {used[resource]:.1f} percent used')
sys.exit(0 if (used[resource] >= percent if comparison == 'at-least' else used[resource] <= percent) else 1)
