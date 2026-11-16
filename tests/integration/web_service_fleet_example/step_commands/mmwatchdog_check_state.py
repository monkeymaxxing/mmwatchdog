#!/usr/bin/env python3
"""Succeeds when this guest's MMWatchdog shows one check in one of the given
states, and, when asked, with the given result of its latest run and a
message or standard output that matches. Prints what MMWatchdog shows.

  python3 step_commands/mmwatchdog_check_state.py CHECK_GROUP/CHECK --state ok [--state ...]
      [--latest-run ok|failed|unknown] [--message-or-output-matches REGEX]

A shell check's message is only its exit code, such as "command exited 0";
what its script printed is the standard output of the run.
"""
import argparse
import json
import re
import subprocess
import sys

parser = argparse.ArgumentParser()
parser.add_argument('check', help='CHECK_GROUP/CHECK, as mmwatchdog status names it')
parser.add_argument('--state', action='append', required=True, help='ok, confirming_failure, failed, confirming_recovery, or unknown')
parser.add_argument('--latest-run', choices=['ok', 'failed', 'unknown'])
parser.add_argument('--message-or-output-matches',
                    help='a regular expression for the message or the standard output of the latest run, each line on its own')
arguments = parser.parse_args()

status = json.loads(subprocess.run(['/usr/local/bin/mmwatchdog', 'status', '--json'], capture_output=True, text=True).stdout)
check = status['checks'][arguments.check]
latest = check.get('last_observation') or {}
text = (latest.get('message') or '') + '\n' + (latest.get('stdout') or '')
print(f'{arguments.check}: state {check["state"]}, latest run {latest.get("status")}: {text.strip()}')
healthy = (check['state'] in arguments.state
           and (arguments.latest_run is None or latest.get('status') == arguments.latest_run)
           and (arguments.message_or_output_matches is None
                or re.search(arguments.message_or_output_matches, text, re.MULTILINE)))
sys.exit(0 if healthy else 1)
