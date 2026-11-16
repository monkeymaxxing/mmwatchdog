# Look back at measurements

**When to use this page:** you want to know what a node, or an application
on it, did earlier: such as the memory use or the CPU of the node or of
`myapp.service` last night, and how it changed. Or you want a node to keep
these numbers for longer.

**Who acts:** you, or your coding agent. Reading the numbers runs on the
node, with an account that can use `sudo`; the commands only read. Adding a
check or changing a setting runs on your controller machine, the machine
where you run `mmx`.

**What this page does:** you make sure that a check measures the number
you want, then read its past values with `mmwatchdog readings`, as a list
or as a chart, for the time that you choose.

**Terms:** a **measurement** is a named number that a check produces, such
as `memory_used_percent`. A **reading** is one value of a measurement at
one time. The **measurement history** is where a node keeps the readings
of every scheduled run of every check, on its own disk, for a time and up
to a size that the node's settings state. A **check that only records**
has `only_record_readings: true`: it stores its readings and never alerts.

## Choose what to do

| You want | Follow |
|---|---|
| The node's CPU, memory, or disk use. The starter check group `machine` records them already | [Read the readings](#read-the-readings) |
| An application's CPU or memory, or another number that no check measures yet | [Record a number](#record-a-number), then [Read the readings](#read-the-readings) |
| Readings from further back than 7 days | [Keep readings for longer](#keep-readings-for-longer) |
| Readings of several nodes, or of a node that no longer runs | [Read the readings of every node on the hub](#read-the-readings-of-every-node-on-the-hub) |

## Before you start

- MMWatchdog runs on the node, installed with MMX from the MMX project on
  your controller machine. If not, follow
  [Install MMWatchdog on one node](install-on-one-node.md) first.
- A node keeps only the readings of the checks that it runs, from the time
  the check was installed. A number that no check measured cannot be read
  later, so [record a number](#record-a-number) before you need it.
- `sudo mmwatchdog status` on the node names the check groups and checks.

## Record a number

Each check that measures, with or without a limit, stores its readings. A
check with a limit, such as `memory_is_below_90_percent`, also alerts. To
store a number without an alert, write a check that only records.

1. **Write the check that only records.** Create a file in
   `files/hub_and_standalone/check_groups/` of the MMX project. This
   example records the CPU and the memory of `myapp.service`:

   ```yaml
   # files/hub_and_standalone/check_groups/myapp_usage.yml
   check_groups:
     myapp_usage:
       check_every: 30s                    # one reading of each measurement every 30 seconds
       time_limit_per_run: 5s
       alert_after_failing_for: 1m
       recovered_after_healthy_for: 30s
       checks:
         cpu_is_recorded:
           only_record_readings: true      # store the readings, never alert
           systemd_unit_resource:
             systemd_unit: myapp.service
             resource: cpu
         memory_is_recorded:
           only_record_readings: true
           systemd_unit_resource:
             systemd_unit: myapp.service
             resource: memory
   ```

   - `check_every` sets how close together the readings are. Every 30
     seconds catches a spike that lasts a minute, and keeps the
     measurement history small.
   - A check that only records states no limit and no `alert_severity`.
     The two waiting times are still required on the check group, but do
     nothing for such a check.
   - Each module page lists its measurements: the
     [`systemd_unit_resource`](../reference/modules/systemd_unit_resource.md)
     page has `cpu_used_percent` and `memory_used_mib`, and the
     [`node_resource`](../reference/modules/node_resource.md) page has those of the
     whole node.

   **Check:** every check has `only_record_readings: true`, or a limit and
   an `alert_severity`.

2. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists your new file as a change to
   `/etc/mmwatchdog`:

   ```text
     install / node / install_monitoring/activate/start_maintenance: succeeded, would_change — would run: tracked input /etc/mmwatchdog/check_groups/myapp_usage.yml would change
   ```

   The dryrun of this project exits with code 2: something would change.

3. **Install the check:**

   ```sh
   mmx run hub_and_standalone
   ```

   **Check:** the run ends with `Run ...: succeeded`. If it ends with
   `failed`, see [If something is different](#if-something-is-different).

4. **Confirm that the check measures.** On the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status myapp_usage
   ```

   ```text
   myapp_usage/cpu_is_recorded  ok           cpu_used_percent is 0 over 0.435s
     Config: /etc/mmwatchdog/check_groups/myapp_usage.yml:9:7
     Rule: check every 30s, time limit per run 5s, alert after failing for 1m, recovered after healthy for 30s
     Sample window: 434.633649ms
     Observed: 2026-11-15T07:26:01.66917046Z
     cpu_used_percent: 0.003911340053655165 percent
   myapp_usage/memory_is_recorded ok           memory_used_mib is 12.59
   ...
   ```

   **Check:** each check shows `ok` and a value. From now on, each run
   adds one reading to the measurement history.

## Read the readings

Run these commands on the node. The examples read the check group
`machine`, which the starter configuration installs, and `myapp_usage`
from [Record a number](#record-a-number).

1. **List the readings of the last hour:**

   ```sh
   sudo mmwatchdog readings machine memory_is_below_90_percent
   ```

   ```text
   check group machine · Nov 15 06:18:24 to 07:18:24 UTC

   memory_is_below_90_percent
   memory_used_percent, percent · fails above 90

     44 readings, none beyond the limit
       07:02:16   18.56
       07:02:42   19.43
   ...
       07:06:14   23.55
   ...
       07:17:25   18.64
       07:17:55   17.96
     lowest 17.96 · highest 23.55 at 07:06:14 · latest 17.96 at 07:17:55
   ```

   The first line names the check group and the time span, in UTC. Then
   comes one block for each measurement: the check, the measurement, its
   unit, and its limit; the number of readings and how many were beyond
   the limit; every reading, oldest first; and the lowest, highest, and
   latest reading.

   - Without `--last`, `--since`, or `--until`, the command shows the last
     hour.
   - Leave out the check, such as `sudo mmwatchdog readings machine`, to
     see every measurement of the check group.
   - The list is never shortened, because one short spike may explain a
     problem. Choose a shorter time span when the list is too long.

   **Check:** the command exits with 0, also when readings were beyond
   their limit. It exits with 4 when it cannot read, such as for a check
   group that does not exist; the message says why.

2. **Choose the time span.** `--last` takes a duration back from now, such
   as `30m`, `12h`, or `7d`:

   ```sh
   sudo mmwatchdog readings myapp_usage --last 12h
   ```

   For a span in the past, such as last night, give its start and end, in
   UTC:

   ```sh
   sudo mmwatchdog readings myapp_usage memory_is_recorded --since 2026-11-15T07:09:40Z --until 2026-11-15T07:11:10Z
   ```

   ```text
   check group myapp_usage · Nov 15 07:09:40 to 07:11:10 UTC

   memory_is_recorded
   memory_used_mib, MiB · only records

     17 readings
       07:09:40   18.25
       07:09:45   18.25
       07:09:50   18.25
       07:09:55   18.12
       07:10:00   18.12
       07:10:10   10.35
       07:10:15   10.35
   ...
       07:11:01   11.25
       07:11:06   11.25
     lowest 10.35 · highest 18.25 at 07:09:40 · latest 11.25 at 07:11:06
   ```

   - `--since` and `--until` take a time in the form
     `2026-11-15T07:09:40Z`, which is UTC. `--since` without `--until`
     ends now. `--until` needs `--since`, and `--last` and `--since`
     cannot be used together; the command says so and exits with 4:

     ```text
     mmwatchdog: --until needs --since; state the start of the time span
     ```
   - The readings at the start and the end time are included.
   - In this list, the memory drops at 07:10: the node rebooted, and
     `myapp.service` started again with less memory.

   **Check:** the first line shows the span that you meant. If the span
   holds no readings, the output says
   `no readings in this time span; choose a longer --last or an earlier --since`.

3. **Draw a chart,** to see how the number changed:

   ```sh
   sudo mmwatchdog readings myapp_usage cpu_is_recorded --chart --since 2026-11-15T07:05:00Z --until 2026-11-15T07:15:00Z
   ```

   ```text
   check group myapp_usage · Nov 15 07:05:00 to 07:15:00 UTC

   cpu_is_recorded
   cpu_used_percent, percent · only records

     7.077┤                                            ╭────────────────────╮
          │                                            │                    │
          │                                            │                    │
          │                                            │                    │
          │                                            │                    ╰╮
          │                ╭─────────────────╮         │                     │
          │                │                 │         │                     │
          │                │                 │         │                     │
          │                │                 │         │                     │
          │                │                 │         │                     │
          │                │                 │         │                     │
         0┤────────────────╯                 ╰─────────╯                     ╰────
          └───────────────────────────────────────────────────────────────────────
           07:05                            07:10                            07:15
     one column is 8s

     121 readings
     lowest 0 · highest 7.077 at 07:12:32 · latest 0.0042 at 07:14:59
   ```

   How to read the chart:

   - **Time runs across.** Each column is an equal slice of the span, as
     the line `one column is 8s` says. The chart is as wide as the
     terminal, or 80 characters when the output goes to a file or a pipe.
   - **The value runs up,** from the lowest to the highest reading or
     limit, which the labels on the left name.
   - **No spike disappears.** When a column holds several readings, it
     shows the one closest to the limit or farthest beyond it; for a check
     that only records, the highest.
   - **A gap stays visible.** Where the readings stop for more than twice
     the usual time between them, such as while the node was down, the
     line breaks off.
   - On a terminal, the line is red where it is beyond the limit. To turn
     color off, set `NO_COLOR=1`, such as
     `sudo NO_COLOR=1 mmwatchdog readings machine --chart`.

   **Check:** the chart has one drawing for each measurement, and under
   it the number of readings and the lowest, highest, and latest reading.

4. **Give the readings to a script or a coding agent,** with every time
   and value in full:

   ```sh
   sudo mmwatchdog readings myapp_usage memory_is_recorded --last 12s --json
   ```

   ```text
   {
     "since": "2026-11-15T07:11:50.3498434Z",
     "until": "2026-11-15T07:12:02.3498434Z",
     "check_group": "myapp_usage",
     "check": "memory_is_recorded",
     "nodes": [
       {
         "measurements": [
           {
             "check_group": "myapp_usage",
             "check": "memory_is_recorded",
             "measurement": "memory_used_mib",
             "readings": [
               {
                 "at": "2026-11-15T07:11:52.004802874Z",
                 "value": 12.5625,
                 "unit": "MiB",
                 "limit_status": "only records"
               },
   ...
   ```

   `--json` and `--chart` cannot be used together: the JSON holds every
   reading. `limit_status` is
   `above the limit`, `below the limit`, `within the limits`, or
   `only records`.

   **Check:** the output is one JSON object.

## Keep readings for longer

Each node keeps its readings for the time in `keep_measurement_history_for`,
up to the disk space in `measurement_history_size_limit`. The starter
settings keep 7 days in at most 256 MiB. When the readings reach the size
limit first, the oldest go first.

1. **Set the time and the size.** In the MMX project, edit
   `files/hub_and_standalone/mmwatchdog_node_settings.yml`:

   ```yaml
   keep_measurement_history_for: 30d         # keep every reading for 30 days
   measurement_history_size_limit: 1GiB      # and never more than 1 GiB of this node's disk
   ```

   - Reason for a larger size with a longer time: the readings take room
     in proportion to the time. A node with ten checks that measure every
     30 seconds collects about 7 MiB of readings a day, so 30 days take
     about 210 MiB.
   - The size is a whole number with `MiB` or `GiB`. Both settings are
     required.
   - MMWatchdog never replaces old readings with averages: a spike on the
     last day of the 30 is still there.

   **Check:** both settings are in the file, each once.

2. **Preview the deploy:**

   ```sh
   mmx dryrun hub_and_standalone
   ```

   **Check:** the output lists
   `/etc/mmwatchdog/mmwatchdog_node_settings.yml` as `would_change`, and the
   dryrun exits with code 2.

3. **Install the change:**

   ```sh
   mmx run hub_and_standalone
   ```

   The new settings apply at once. A shorter time removes the older
   readings right after the run; a lower size limit removes the oldest
   readings in small steps, so that no check waits.

   **Check:** the run ends with `Run ...: succeeded`.

4. **Confirm how far back the node can look.** On the node:

   ```sh
   ssh deploy@app01 sudo mmwatchdog status --json
   ```

   The part `measurement_history` says how far back the node can look:

   ```text
   ...
     "measurement_history": {
       "oldest_reading_at": "2026-11-15T07:02:16.644477843Z",
       "newest_reading_at": "2026-11-15T07:19:01.089104419Z",
       "keep_measurement_history_for": "30d",
       "measurement_history_size_limit": "1GiB",
       "cut_short_by_measurement_history_size_limit": false
     },
   ...
   ```

   **Check:** the two settings show your values. `oldest_reading_at` is
   the first reading that the node holds: the history reaches back 30 days
   only once the node has run for 30 days.

When the size limit has removed readings that are younger than
`keep_measurement_history_for`, `sudo mmwatchdog status` prints one line,
such as:

```text
measurement history reaches back 18 minutes 7 seconds; the size limit of 1 MiB prevented keeping the requested 30 days
```

The line is information: it does not change the exit code of `status`.
`sudo mmwatchdog status --json` then has
`"cut_short_by_measurement_history_size_limit": true`.
To keep the full time from now on, raise `measurement_history_size_limit`
and run `mmx run hub_and_standalone`; the line goes at once.

## Read the readings of every node on the hub

In a fleet, each joined node also sends its readings to the fleet hub,
with each report. The hub keeps them in its **measurement history on the
hub**, so the readings of several nodes are in one place, also those of a
node that no longer runs. Each node keeps its own readings as well.

1. **Set how long the hub keeps the readings.** Two settings of
   `fleet_hub_role`, in the node settings of the hub, are required:

   ```yaml
   # files/hub_and_standalone/mmwatchdog_node_settings.yml, on the hub
   fleet_hub_role:
     report_every: 30s
     alert_after_hub_unreachable_for: 3m
     send_alert_without_hub_after_waiting_for: 30s
     undelivered_alerts_of_nodes_content_size_limit: 32MiB
     listen_on_address: 0.0.0.0:8445
     address_for_nodes: util.example.com:8445
     keep_measurement_history_of_nodes_for: 7d         # the hub removes a reading when it is 7 days old
     measurement_history_of_nodes_size_limit: 20GiB    # all nodes' readings together never take more than 20 GiB
   ```

   The hub's own readings stay in its own measurement history, under its
   own `keep_measurement_history_for`.
   [Deploy a fleet](set-up-a-fleet.md) covers the other
   settings.

   **Check:** after `mmx run hub_and_standalone`, `sudo mmwatchdog fleet`
   on the hub lists the joined nodes.

2. **Read one node's readings.** On the hub:

   ```sh
   sudo mmwatchdog fleet readings app01 machine memory_is_below_90_percent --last 12h
   ```

   The command takes the same time options, `--chart`, and `--json` as
   `mmwatchdog readings`, and its output has the same form.

   **Check:** the output lists the node's readings. A node that could not
   reach the hub for a while catches up: it sends the readings that the
   hub missed, oldest first, with its next reports.

3. **Compare one check across a node group:**

   ```sh
   sudo mmwatchdog fleet readings --node-group web machine memory_is_below_90_percent --last 12h
   ```

   The node with the most readings beyond the limit comes first, then the
   nodes without readings in the span, which may have stopped reporting.
   The healthy nodes share one line, such as
   `12 healthy, none beyond the limit: app-1, app-2, ...`. With `--chart`,
   one chart shows all nodes: a shaded band from the lowest to the highest
   node in each column, and a line for the node closest to the limit or
   farthest beyond it.

   **Check:** each node of the node group appears once. A node group is a
   named selection of nodes; [Measurement history on the hub](../reference/configuration-files.md#measurement-history)
   lists every rule of the hub's readings.

## If something is different

- **The run failed at `validate_config`, because a check has neither a
  limit nor `only_record_readings`.** The output names the file and the
  line:

  ```text
  mmwatchdog: /usr/local/lib/mmwatchdog/candidate_config/check_groups/myapp_usage.yml:14:7: check_groups.myapp_usage.checks.memory_is_recorded.only_record_readings: is required without alert_severity: choose whether to record or alert; add only_record_readings: true, or add alert_severity: warn and a limit for a module that needs one
  ```

  Add `only_record_readings: true` to record, or a limit and an
  `alert_severity` to alert. A check with `only_record_readings: true`
  and an `alert_severity` or a limit is refused as well:
  `a check that only records readings has no limits or alert severity`.
- **The readings have a limit.** A check with a limit stores the limit
  with each reading. A reading beyond the limit says `above the limit` or
  `below the limit`; a reading equal to the limit is within it. The chart
  draws the limit as a dashed line and marks each column beyond it with
  `▲`. Each run of readings beyond the limit gets one line under the
  chart. This check, `cpu_is_below_5_percent` in the check group
  `myapp_cpu`, measures the CPU of `myapp.service` with
  `fail_when_measurement_above: {cpu_used_percent: 5}`. It was installed
  at 07:15:24, so the legend says `no readings before 07:15:24`:

  ```sh
  sudo mmwatchdog readings myapp_cpu --chart --since 2026-11-15T07:14:00Z --until 2026-11-15T07:19:00Z
  ```

  ```text
  check group myapp_cpu · Nov 15 07:14:00 to 07:19:00 UTC

  cpu_is_below_5_percent
  cpu_used_percent, percent · fails above 5

    7.017┤ ╭──────────────────────────────────────╮
         │ │                                      │
         │ │                                      │
        5┤╌│╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╰╮╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌
         │ │                                       │
  ...
        0┤─╯                                       ╰─────────────────
         └─▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲▲──────────────────────────────
          07:15:24                       07:17:12                        07:19:00
    ╌ limit   ▲ beyond the limit
    one column is 3s · no readings before 07:15:24

    36 readings, 24 beyond the limit
      07:15:30 to 07:17:26   24 readings above the limit, farthest 7.017 at 07:16:05
    lowest 0 · highest 7.017 at 07:16:05 · latest 0.004657 at 07:18:12
  ```

  When a limit changed within the span, each reading is judged by the
  limit stored with it, and the title says `limits vary`.
- **The check group or the check does not exist.** The command exits
  with 4:

  ```text
  mmwatchdog: unknown check group "nosuch"; use mmwatchdog status or mmwatchdog fleet NODE to see the check groups
  ```
- **A reading is missing.** Only scheduled runs that measured something
  store readings, also runs that failed their limit. A check that measures
  nothing, such as `tcp`, stores none. `mmwatchdog check-now` stores none, a run during a
  maintenance window stores its readings, and a run that cannot measure,
  such as a CPU check of a stopped service, is `unknown` and stores
  none. A check that only records opens no alert for an `unknown` run.
- **MMWatchdog cannot store a reading,** such as on a full disk. The check
  goes on as before, and `sudo mmwatchdog status` reports a storage
  problem and exits with 3.
  [When a reading cannot be stored](../when-something-fails.md#a-reading-or-the-nodes-state-cannot-be-saved)
  says what to do.
- **A calculation that `readings` does not give.** The measurement
  history is the SQLite file `/var/lib/mmwatchdog/measurement_history.db`,
  which only root can read.
  [Looking at the readings](look-back-at-measurements.md#read-the-readings) describes
  its table.

## Related pages

- [Add a check](add-a-check.md)
- [See what is happening](see-what-is-happening.md): the state of each
  check now, and the events of the past.
- [Decide when a check alerts](decide-when-a-check-alerts.md), for a check
  with a limit.
- The [module pages](../reference/modules/README.md): each lists its measurements.
