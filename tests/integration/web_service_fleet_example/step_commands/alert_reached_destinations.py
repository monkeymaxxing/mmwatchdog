#!/usr/bin/env python3
"""Succeeds when the newest alert of one type of one check of this guest's
MMWatchdog reached exactly the named destinations. Every destination of the
example posts to the fixture's receiver on the same guest at the path
/events/<destination>, and the receiver records each alert with its path in
/var/tmp/mmwatchdog-fixture/webhooks.jsonl.

  python3 step_commands/alert_reached_destinations.py CHECK_GROUP/CHECK TYPE DESTINATION[,DESTINATION...]
"""
import json
import sys

check_name, alert_type, destinations = sys.argv[1:]
check_group, check = check_name.split('/')
expected = sorted(destinations.split(','))
with open('/var/tmp/mmwatchdog-fixture/webhooks.jsonl') as file:
    # A line that the receiver is still writing has no newline yet.
    records = [json.loads(line) for line in file if line.endswith('\n')]
alerts = [record for record in records if record['accepted'] and record['event'].get('check_group') == check_group
          and record['event'].get('check') == check and record['event']['type'] == alert_type]
if not alerts:
    print(f'no {alert_type} alert of {check_name} has arrived')
    sys.exit(1)
newest = alerts[-1]['event']['id']
reached = sorted({record['path'].removeprefix('/events/') for record in alerts if record['event']['id'] == newest})
print(f'the newest {alert_type} alert of {check_name} reached {", ".join(reached)}; expected {", ".join(expected)}')
sys.exit(0 if reached == expected else 1)
