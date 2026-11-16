#!/usr/bin/env python3
"""Exercise workload transitions, optionally continuing to a sustained steady state."""
import argparse
from collections import defaultdict
from contextlib import closing
import json
from pathlib import Path
import sqlite3
import subprocess
import time

FIXTURE = Path('/var/tmp/mmwatchdog-fixture')
# The sustained phase fills this event history and then turns half of it over.
EVENT_HISTORY_SIZE_LIMIT = '2MiB'
FLIPPING = [f'shell_{i}' for i in range(24)]


def run(args, check=True):
    result = subprocess.run(args, text=True, capture_output=True, timeout=20)
    if check and result.returncode:
        raise RuntimeError(str(args) + ': ' + result.stderr)
    return result


def status():
    result = run(['/usr/local/bin/mmwatchdog', 'status', '--json'], False)
    data = json.loads(result.stdout)
    assert not data.get('storage_error'), data
    assert not data['delivery']['delivery_gaps'], data
    return data


def sample(elapsed):
    properties = dict(line.split('=', 1) for line in run(['systemctl', 'show', 'mmwatchdog',
        '--property=MainPID,MemoryCurrent,CPUUsageNSec,NRestarts,InvocationID']).stdout.splitlines())
    pid = int(properties['MainPID'])
    stat = dict(line.split(':', 1) for line in Path(f'/proc/{pid}/status').read_text().splitlines())
    return {'seconds': round(elapsed, 2), 'pid': pid, 'invocation': properties['InvocationID'],
            'memory_bytes': int(properties['MemoryCurrent']), 'rss_bytes': int(stat['VmRSS'].split()[0]) * 1024,
            'cpu_ns': int(properties['CPUUsageNSec']), 'fds': len(list(Path(f'/proc/{pid}/fd').iterdir())),
            'threads': int(stat['Threads']), 'unexpected_restarts': int(properties['NRestarts']),
            'storage_bytes': sum(p.stat().st_size for p in Path('/var/lib/mmwatchdog').iterdir() if p.is_file())}


def history(since=0):
    """Return the retained event count, the latest event sequence number, the
    capacity misses recorded after sequence number since, the oldest retained
    sequence number, and the retained bytes."""
    with closing(sqlite3.connect('file:/var/lib/mmwatchdog/state.db?mode=ro', uri=True)) as db:
        return db.execute("""SELECT count(*), coalesce(max(seq), 0),
            count(*) FILTER (WHERE seq > ? AND json_extract(data, '$.message') LIKE '%execution capacity unavailable%'),
            coalesce(min(seq), 0), coalesce(sum(length(data)), 0)
            FROM events""", (since,)).fetchone()


def median(values):
    return sorted(values)[len(values) // 2]


def accepted_events(reports):
    # Delivery is at least once. A lost acknowledgment may repeat an event, but
    # its identity and contents must remain stable so receivers can deduplicate.
    events = {}
    for report in reports:
        if not report['accepted']: continue
        event = report['event']
        previous = events.setdefault(event['id'], event)
        assert previous == event, 'event changed across retries'
    return list(events.values())


def main(sustained):
    assert Path('/etc/mmwatchdog-test-vm').read_text().strip() == 'mmwatchdog-disposable-integration-v1'
    # The sustained phase flips these together to fill and turn over history.
    # Every observation is one committed write. Flipping these together queues
    # their commits at once; the interval leaves room for that burst.
    check_groups = {name: {'check_every': '500ms', 'time_limit_per_run': '250ms', 'alert_after_failing_for': '0s', 'recovered_after_healthy_for': '0s',
                     'shell': 'test ! -e /var/tmp/mmwatchdog-fixture/flip', 'alert_severity': 'warn'} for name in FLIPPING}
    # The other check groups run every second and confirm for one second.
    timings = {'check_every': '1s', 'time_limit_per_run': '500ms', 'alert_after_failing_for': '1s', 'recovered_after_healthy_for': '1s'}
    check_groups.update({
        'http': dict(timings, http={'url': 'http://127.0.0.1:18080/health', 'response_body_contains': 'ready'}, alert_severity='warn'),
        'cpu': dict(timings, node_resource='cpu', only_record_readings=True), 'memory': dict(timings, node_resource='memory', only_record_readings=True),
        'disk': dict(timings, node_filesystem='/', only_record_readings=True),
        'mmwatchdog_resources': dict(timings, checks={
            'systemd_unit_state': {'systemd_unit_state': {'systemd_unit': 'mmwatchdog.service', 'fail_when': 'the_unit_is_not_active'}, 'alert_severity': 'sos'},
            'systemd_unit_crash_loop': {'systemd_unit_crash_loop': {'systemd_unit': 'mmwatchdog.service', 'automatic_restarts_above': 2, 'automatic_restarts_within': '1h'}, 'alert_severity': 'warn'},
            'cpu': {'only_record_readings': True, 'systemd_unit_resource': {'systemd_unit': 'mmwatchdog.service', 'resource': 'cpu'}},
            'memory': {'only_record_readings': True, 'systemd_unit_resource': {'systemd_unit': 'mmwatchdog.service', 'resource': 'memory'}}}),
    })
    # The measurement history gets 1 MiB, which the checks fill in minutes:
    # from then on the oldest readings go, and the storage that the soak
    # watches for growth stays level.
    config = {'keep_undelivered_alerts_for': '24h', 'undelivered_alerts_content_size_limit': '8MiB', 'alert_severity_when_this_node_goes_silent': 'sos', 'alert_severity_when_this_node_has_rebooted': 'info',
              'keep_measurement_history_for': '7d', 'measurement_history_size_limit': '1MiB',
              'event_history_content_size_limit': EVENT_HISTORY_SIZE_LIMIT}
    Path('/etc/mmwatchdog/mmwatchdog_node_settings.yml').write_text(json.dumps(config))
    alerts = {'destinations': {'capture': {'remind_every': 'never', 'webhook': {'url': 'http://127.0.0.1:18080/events'}, 'time_limit_per_delivery_attempt': '1s', 'alert_severities': ['info', 'warn', 'sos']}}}
    Path('/etc/mmwatchdog/alert_destinations.yml').write_text(json.dumps(alerts))
    for path in Path('/etc/mmwatchdog/check_groups').glob('*.yml'): path.unlink()
    Path('/etc/mmwatchdog/check_groups/node.yml').write_text(json.dumps({'check_groups': check_groups}))
    for name in ['fail', 'reject', 'flip']: (FIXTURE / name).unlink(missing_ok=True)
    (FIXTURE / 'webhooks.jsonl').write_text('')
    # Start from empty state so earlier runs leave no incidents or queue.
    run(['systemctl', 'stop', 'mmwatchdog'])
    for name in ['state.db', 'state.db-wal', 'state.db-shm']: Path('/var/lib/mmwatchdog', name).unlink(missing_ok=True)
    run(['systemctl', 'start', 'mmwatchdog'])
    samples, statuses, expected_incidents = [], [], []
    start = time.monotonic()
    next_sample, restarts = 0, 0

    def reports():
        # A receiver may be appending the last line while we poll.
        return [json.loads(line) for line in (FIXTURE / 'webhooks.jsonl').read_text().splitlines(keepends=True)
                if line.endswith('\n')]

    def observe():
        nonlocal next_sample
        elapsed = time.monotonic() - start
        data = status()
        statuses.append(data['delivery']['queued_notifications'])
        if elapsed >= next_sample:
            measurement = sample(elapsed)
            assert measurement['unexpected_restarts'] == 0, measurement
            assert measurement['storage_bytes'] < 160 * 1024 * 1024, measurement
            samples.append(measurement)
            with (FIXTURE / 'soak-samples.jsonl').open('a') as stream:
                stream.write(json.dumps(measurement) + '\n')
            next_sample = elapsed + 1
        return data

    def wait_for(predicate, description, seconds=20):
        deadline = time.monotonic() + seconds
        data = None
        while time.monotonic() < deadline:
            try:
                data = observe()
            except json.JSONDecodeError:
                # systemd may acknowledge startup before the socket is ready.
                time.sleep(.1)
                continue
            if predicate(data): return data
            time.sleep(.1)
        raise AssertionError(f'timed out waiting for {description}: {data}')

    def healthy(data):
        # mmwatchdog_resources contributes four checks: systemd_unit_state,
        # systemd_unit_crash_loop, cpu and memory.
        return len(data['checks']) == len(check_groups) + 3 and all(
            c['state'] == 'ok' and not c['needs_fresh_observation'] and c['last_observation']['status'] == 'ok'
            for c in data['checks'].values())

    def restart():
        nonlocal restarts, next_sample
        before = run(['systemctl', 'show', 'mmwatchdog', '--property=InvocationID', '--value']).stdout
        run(['systemctl', 'restart', 'mmwatchdog'])
        assert run(['systemctl', 'show', 'mmwatchdog', '--property=InvocationID', '--value']).stdout != before
        restarts += 1
        next_sample = 0

    def http_cycle():
        """Fail and recover the HTTP check while its notifications are rejected.
        The first two cycles also restart the daemon with notifications queued."""
        (FIXTURE / 'reject').touch()
        (FIXTURE / 'fail').touch()
        failed = wait_for(lambda d: bool(d['checks']['http/http']['failure_incident'].get('incident_id')),
                          'confirmed HTTP failure')
        incident = failed['checks']['http/http']['failure_incident']['incident_id']
        assert incident not in expected_incidents
        expected_incidents.append(incident)
        wait_for(lambda d: d['delivery']['queued_notifications'] > 0 and sum(
            not r['accepted'] and r['event'].get('incident_id') == incident for r in reports()) >= 2,
            'rejected notification and retry')
        if len(expected_incidents) == 1:
            restart()
            wait_for(lambda d: d['checks']['http/http']['failure_incident'].get('incident_id') == incident
                     and not d['checks']['http/http']['needs_fresh_observation'] and d['delivery']['queued_notifications'] > 0,
                     'active incident and queued notification after restart')
        (FIXTURE / 'fail').unlink()
        wait_for(lambda d: healthy(d) and d['delivery']['queued_notifications'] >= 2, 'confirmed recovery queued behind failure')
        if len(expected_incidents) == 2:
            restart()
            wait_for(lambda d: healthy(d) and d['delivery']['queued_notifications'] >= 2,
                     'queued failure and recovery after restart')
        (FIXTURE / 'reject').unlink()
        # MMWatchdog doubles its wait after each attempt that the receiver rejects:
        # 1, 2, 4, 8, 16, 32 seconds. On a slow guest the steps above take
        # long enough for a fifth attempt, and the next one comes 32
        # seconds later. The wait ends as soon as the queue is empty.
        return wait_for(lambda d: healthy(d) and d['delivery']['queued_notifications'] == 0, 'ordered delivery and queue drain', seconds=90)

    def sustain():
        """Flip the shell checks in maintenance until history has reached its
        limit and half of it has turned over, so pruning runs on every commit. Their
        transitions are recorded but not delivered, keeping the queue below its
        limit; HTTP cycles keep exercising delivery. Resource use is compared
        within that steady state, where it should stay flat."""
        for name in FLIPPING:
            run(['/usr/local/bin/mmwatchdog', 'maintenance', 'on', name, '--for', '168h'])
        flipping = lambda d, open_: all(bool(d['checks'][n + '/shell']['failure_incident'].get('incident_id')) == open_
                                        and not d['checks'][n + '/shell']['needs_fresh_observation'] for n in FLIPPING)
        wait_for(lambda d: flipping(d, False), 'fresh observations in maintenance')
        # Daemon starts during coverage can miss a slot while every check
        # is due at once; the sustained load itself must not.
        _, begun, _, first_oldest, _ = history()
        limit_bytes = int(EVENT_HISTORY_SIZE_LIMIT.removesuffix('MiB')) * 1024 * 1024
        deadline = time.monotonic() + 1500
        steady, rounds, cycles = None, 0, 0
        while True:
            assert time.monotonic() < deadline, ('history did not turn over', history())
            (FIXTURE / 'flip').touch()
            wait_for(lambda d: flipping(d, True), 'flipped checks failed')
            (FIXTURE / 'flip').unlink()
            wait_for(lambda d: flipping(d, False), 'flipped checks recovered')
            rounds += 1
            count, latest, missed, oldest, retained_bytes = history(begun)
            assert missed == 0, f'{missed} observations missed for lack of execution capacity; lower the workload rate'
            assert retained_bytes <= limit_bytes, (retained_bytes, limit_bytes)
            # The history is full once the size limit has removed its first events.
            if steady is None and oldest > first_oldest:
                steady = {'sample': len(samples), 'seq': latest, 'count': count}
            if steady is not None and latest - steady['seq'] >= steady['count'] // 2:
                break
            if rounds % 50 == 0:
                http_cycle()
                cycles += 1
        for name in FLIPPING:
            run(['/usr/local/bin/mmwatchdog', 'maintenance', 'off', name])
        window = samples[steady['sample']:]
        assert len(window) >= 20, len(window)
        quarter = len(window) // 4
        first, last = window[:quarter], window[-quarter:]
        result = {'flip_rounds': rounds, 'http_cycles': cycles, 'events_recorded': latest,
                  'steady_samples': len(window),
                  'steady_rss_bytes': [median([s['rss_bytes'] for s in part]) for part in (first, last)],
                  'steady_storage_bytes': [max(s['storage_bytes'] for s in part) for part in (first, last)],
                  # Executions briefly hold pipes, so compare typical counts, not peaks.
                  'steady_fds': [median([s['fds'] for s in part]) for part in (first, last)],
                  'steady_threads': [median([s['threads'] for s in part]) for part in (first, last)]}
        rss, storage, fds = result['steady_rss_bytes'], result['steady_storage_bytes'], result['steady_fds']
        assert rss[1] - rss[0] < 8 * 1024 * 1024, result
        assert storage[1] - storage[0] < 4 * 1024 * 1024, result
        assert fds[1] <= fds[0] + 2, result
        return result

    initial = wait_for(lambda d: healthy(d) and d['delivery']['queued_notifications'] == 0, 'initial healthy observations')
    (FIXTURE / 'webhooks.jsonl').write_text('')
    # Each step advances on observed behavior, with bounded failure deadlines.
    while len(expected_incidents) < 2:
        final = http_cycle()
    steady_state = sustain() if sustained else None
    if sustained:
        final = wait_for(lambda d: healthy(d) and d['delivery']['queued_notifications'] == 0, 'healthy after maintenance')

    elapsed = time.monotonic() - start
    assert restarts == 2
    assert final['delivery']['queued_notifications'] == 0, final['delivery']
    assert healthy(final), final
    assert all(c['last_observation']['at'] != initial['checks'][name]['last_observation']['at']
               for name, c in final['checks'].items()), 'observations stopped progressing'
    deliveries = reports()
    accepted = [r['event'] for r in deliveries if r['accepted']]
    unique = accepted_events(deliveries)
    incidents = defaultdict(list)
    for event in unique:
        if event.get('check_group') == 'http': incidents[event['incident_id']].append(event['type'])
    assert set(incidents) == set(expected_incidents), incidents
    assert all(events == ['failed', 'recovered'] for events in incidents.values()), incidents
    assert {r['event']['id'] for r in deliveries if not r['accepted']} <= {e['id'] for e in unique}, 'lost queued delivery'
    # Compare warm samples in each daemon lifetime rather than hiding growth
    # behind deliberate restarts. The sustained phase runs in the final process.
    by_process = defaultdict(list)
    for s in samples: by_process[s['invocation']].append(s)
    rss_growth = [group[-1]['rss_bytes'] - group[min(3, len(group)-1)]['rss_bytes'] for group in by_process.values() if len(group) >= 5]
    assert not rss_growth or max(rss_growth) < 32 * 1024 * 1024, rss_growth
    assert max(s['fds'] for s in samples) < 128, samples[-1]
    cpu_ns = sum(max(s['cpu_ns'] for s in group) - min(s['cpu_ns'] for s in group) for group in by_process.values())
    report = {'duration_seconds': elapsed, 'mode': 'sustained' if sustained else 'coverage',
              'steady_state': steady_state,
              'checks': len(final['checks']), 'deliberate_restarts': restarts,
              'completed_incidents': len(incidents), 'accepted_deliveries': len(accepted),
              'unique_delivered_events': len(unique), 'duplicate_deliveries': len(accepted) - len(unique),
              'rejected_deliveries': sum(not r['accepted'] for r in deliveries), 'max_pending': max(statuses),
              'cpu_percent_one_core': cpu_ns / 1e9 / elapsed * 100,
              'rss_peak_bytes': max(s['rss_bytes'] for s in samples),
              'memory_peak_bytes': max(s['memory_bytes'] for s in samples),
              'max_warm_rss_growth_bytes': max(rss_growth, default=0),
              'fds_peak': max(s['fds'] for s in samples),
              'storage_peak_bytes': max(s['storage_bytes'] for s in samples),
              'final_storage_bytes': samples[-1]['storage_bytes'],
              'final_queued_notifications': final['delivery']['queued_notifications'],
              'version': json.loads(run(['/usr/local/bin/mmwatchdog', 'version', '--json']).stdout),
              'time': time.strftime('%Y-%m-%dT%H:%M:%SZ', time.gmtime())}
    (FIXTURE / 'soak-result.json').write_text(json.dumps(report, indent=2) + '\n')
    print(json.dumps(report), flush=True)


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--sustained', action='store_true',
                        help='after coverage, continue until event history has filled and half turned over')
    main(parser.parse_args().sustained)
