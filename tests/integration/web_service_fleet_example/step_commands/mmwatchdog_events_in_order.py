#!/usr/bin/env python3
"""Succeeds when the events of one check group in this guest's MMWatchdog hold
the given types in this order, each after the one before, from the newest
event of the first type on. Prints the events from there.

  python3 step_commands/mmwatchdog_events_in_order.py CHECK_GROUP TYPE TYPE...
"""
import json
import subprocess
import sys

check_group, *types = sys.argv[1:]
events = json.loads(subprocess.run(['/usr/local/bin/mmwatchdog', 'events', check_group, '--json'], capture_output=True, text=True).stdout)
events.sort(key=lambda event: event['at'])
starts = [number for number, event in enumerate(events) if event['type'] == types[0]]
if not starts:
    print(f'no event {types[0]} in {check_group}')
    sys.exit(1)
since = events[starts[-1]:]
print('\n'.join(f'{event["at"]} {event["type"]} {event.get("check", "")} {event.get("message", "")[:160]}' for event in since))
found = [event['type'] for event in since if event['type'] in types]
position = 0
for found_type in found:
    if position < len(types) and found_type == types[position]:
        position += 1
sys.exit(0 if position == len(types) else 1)
