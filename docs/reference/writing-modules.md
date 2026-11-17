# Writing modules reference

This page lists what a module of the project's own may use and must do, for
looking up while you write one: the module's name and directory, the fields
of `module_description.yml`, what a check module returns, the Starlark
language as MMWatchdog runs it, every primitive and what it returns, state
kept between runs, findings, destination modules, the account and the
environment of a module, and the JSON protocol of an executable module with
its Bash helper. To write a first check module and a first destination
module step by step, see
[Write your own check or destination](../how-to/write-your-own-check-or-destination.md).
The modules that come with MMWatchdog have their own pages:
[module pages](modules/README.md).

A **module** is the code that does the work of one check or of one alert
destination, such as `http` or `slack`. A check or a destination names
exactly one module, and writes the module's **inputs** below the module's
name. A module has one of three kinds, which say where the module comes
from:

| Kind | What the module is | Where a node has the module |
| --- | --- | --- |
| Core module | Go code built into the `mmwatchdog` program, such as `http` | In `mmwatchdog` itself |
| Official extra module | A Starlark script that the MMWatchdog project writes, such as `slack` | `/usr/local/lib/mmwatchdog/official_extra_modules/NAME/`, from `official_extra_modules/` of the release |
| Module of the project's own | A Starlark script or an executable that you write | `/usr/local/lib/mmwatchdog/external_modules/NAME/`, from `mmwatchdog_external_modules/` of your project |

A check calls a module of every kind in the same way, so the configuration
never states the kind. Write a [Starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md)
script unless the module needs something that a script cannot do: see
[An executable in place of a script](#an-executable-in-place-of-a-script).

## The module's name

| Rule | Detail |
| --- | --- |
| Characters | Lower-case letters, digits, and underscores. The first character is a letter. At most 64 characters, such as `queued_jobs`. A hyphen is not allowed. |
| Taken names | The name of a core module, of an official extra module, or of a setting that stands beside a module, such as `alert_severity`. A name means one module on every node. |
| In a check or a destination | The module's name as the key, with the inputs below it, as for a core module. |

A directory in `mmwatchdog_external_modules/` with a taken name, or with a
name that no module can have, stops `mmwatchdog validate` and every deploy,
also when nothing names the module:

```text
the module slack in /usr/local/lib/mmwatchdog/external_modules has the name of the official extra module slack, which every deploy installs. A name means one module on every node: give your module another name
```

```text
/usr/local/lib/mmwatchdog/external_modules/queue-length is no module: the name of a module uses lower-case letters, digits, and underscores, starts with a letter, and has at most 64 characters, such as queued_jobs
```

Do not edit an official extra module, and do not copy one into
`mmwatchdog_external_modules/`: the next release brings the module again.
For a module that works differently, write a module of your own under
another name.

## The module's directory

| File | Required | What it holds |
| --- | --- | --- |
| `module_description.yml` | Yes | The module's summary, inputs, measurements, and examples, at most 256 KiB: [The module description](#the-module-description) |
| `module.star` | One of the code files | A Starlark script of at most 1 MiB: [A Starlark script](#a-starlark-script) |
| `module_executable` | One of the code files | Any program that runs on the node, such as a Bash script or a Go program: [An executable in place of a script](#an-executable-in-place-of-a-script) |
| `README.md` | No | The module's page. `mmwatchdog describe-module NAME` prints the page after the description. |

- A directory holds `module.star` or `module_executable`, not both. A
  directory with neither stops validation:
  `the module queued_jobs has no code: ... must hold the script module.star or the executable module_executable`.
- A file in `mmwatchdog_external_modules/` itself, such as the
  `README.md` that comes with the release, is no module.
- Each deploy makes each `module_executable` runnable, with mode 0755,
  whatever its mode on the controller machine.
- Keep `mmwatchdog_external_modules/` in the project even without modules
  of your own. MMX refuses a missing directory before it connects to any
  node, so a missing directory cannot erase the modules on the nodes.

## The module description

Every module has a module description. A module of the project's own keeps
it in `module_description.yml`, beside the script or the executable:

```yaml
# mmwatchdog_external_modules/queued_jobs/module_description.yml
summary: Counts the jobs that wait in a queue file.
used_as: check
inputs:
  queue_file_path:
    type: string
    required: true
    description: The absolute path of the file that holds the number of waiting jobs.
measurements:
  queued_jobs:
    unit: count
    description: The number of jobs that wait.
examples:
  - description: Fail when more than 500 jobs wait.
    config: |
      queued_jobs:
        queue_file_path: /var/lib/myapp/pending
      fail_when_measurement_above:
        queued_jobs: 500
      alert_severity: warn
```

| Field | Required | Meaning |
| --- | --- | --- |
| `summary` | Yes | One sentence that says what the module does. `mmwatchdog describe-module` shows it in its list. |
| `used_as` | Yes | `check`, or `alert_destination` for a [destination module](#destination-modules). |
| `inputs` | No | The module's inputs by name, each with the fields below. |
| `measurements` | No | Only for a check: the measurements that the module reports, by name. Each measurement has a `unit` of at most 64 bytes, such as `count` or `percent`, and a `description`. A measurement's name follows the rule for a module's name. |
| `one_incident_for_each_finding` | No | Only for a check: `true` for a module that reports [findings](#findings). |
| `service_keeps_incidents` | No | Only for a destination: `true` for a module whose service keeps incidents of its own, such as a pager: [A service that keeps incidents](#a-service-that-keeps-incidents). |
| `examples` | Yes, at least one | Each example has a `description` and a `config`: the YAML of one check or one destination that uses the module. |

| Input field | Required | Meaning |
| --- | --- | --- |
| `type` | Yes | One of the types below. |
| `required` | Yes | `true` when every check or destination must state the input, `false` when it may leave the input out. |
| `description` | Yes | What the input holds, with an example value. `mmwatchdog validate` ends its error about a missing required input with this text, so write it for the person who reads that error. |
| `allowed_values` | No | A list of the values the input may have. Only for the types `string` and `integer`. |

| Type | A value of the type |
| --- | --- |
| `string` | Text, such as `/var/lib/myapp/pending`. |
| `integer` | A whole number. |
| `number` | Any number. |
| `boolean` | `true` or `false`. |
| `duration` | A time written as MMWatchdog's own settings write one, such as `30s`, `5m`, `2h`, or `7d`. MMWatchdog checks the form, and the module gets the text as written. |
| `list_of_strings` | A list of texts. |

- An input has no default value. A script reads an input that may be left
  out with `inputs.get("NAME", DEFAULT)`.
- The name of an input follows the rule for a module's name. No input can
  be named `run_as_user_account`, `run_from_directory`, or `env`, because
  these [run settings](#the-account-and-the-environment-of-a-module) stand
  beside the inputs.
- An unknown field, or a key written twice, is an error at its line, such
  as
  `module_description.yml:7: inputs.queue_file_path: unknown field "requird"; the fields are type, required, allowed_values, description`.
  A description with an error makes the module unusable.
- MMWatchdog checks each example against the description when it reads the
  description.
- MMWatchdog reads the description of each module that the configuration
  names, when MMWatchdog starts and at each reload. A module that nothing
  names is never read and never run.

**`mmwatchdog validate` checks every check and every destination against
the description of its module,** before anything runs. Each error starts
with the file and the line of the check:

| The check | `mmwatchdog validate` answers |
| --- | --- |
| States an input that the module does not have | `check_groups.myapp_jobs.checks.job_queue_is_short.queued_jobs.queue_file: unknown field "queue_file": the inputs of the module queued_jobs are queue_file_path` |
| Leaves out a required input | `check_groups.myapp_jobs.checks.job_queue_is_short.queued_jobs.queue_file_path: is required: The absolute path of the file that holds the number of waiting jobs.` |
| States a value of the wrong type | `...queued_jobs.queue_file_path: expected a string` |
| States a value that `allowed_values` does not list | `...: expected one of` and the allowed values |
| States a limit on a measurement that the module does not report | `check_groups.myapp_jobs.checks.job_queue_is_short.fail_when_measurement_above.jobs: this check has no such measurement; it measures queued_jobs` |
| Names a module that is used as an alert destination | `check_groups.myapp_jobs.checks.job_queue_is_short.slack: the module slack is used as an alert destination, not as a check: its description says used_as: alert_destination` |
| Names a module that no directory holds | `unknown field "queued_jobs": the field name matches neither a setting of a check nor an installed module.` and where MMWatchdog looked |

On the node, `mmwatchdog validate` also reads each script that the
configuration names, without running it. A script that is not valid
Starlark, that uses a name that nothing defines, that loads another file,
or that does not define the function that MMWatchdog calls with its
arguments is an error with the file and the line. So such a script stops
a deploy. `mmwatchdog validate --config-only` leaves out the checks that
need the node, such as this one.

## A check module's result

A check module's script defines `run_check(inputs, file_contents)`, and
MMWatchdog calls it once for each run of a check that uses the module.
`inputs` is a dictionary of the check's inputs. `file_contents` holds the
text of each file that an input names: see
[Inputs that name a file](#inputs-that-name-a-file). The function returns a
dictionary:

| Key | Meaning |
| --- | --- |
| `status` | Required. `ok`, `failed`, or `unknown`. |
| `message` | Required. One text that says what the module found, such as `12 queued jobs`. |
| `measurements` | Optional. A dictionary from a measurement's name to its number, such as `{"queued_jobs": 780}`. At most 128. |
| `findings` | Only for a module that reports [findings](#findings). |

**The module measures, and MMWatchdog does the rest.** MMWatchdog owns the
check's schedule, limits, waiting times, incidents, recovery actions, and
alerts.

- **`status`** is `ok` when the module could measure, `failed` when the
  thing that the module checks does not work at all, and `unknown` when the
  module could not measure.
- **The limit belongs to the check.** A check states a limit with
  `fail_when_measurement_above` or `fail_when_measurement_below`.
  MMWatchdog applies the limits to an `ok` result: a reading strictly above
  its `above` number, or strictly below its `below` number, fails the
  observation, with a message such as
  `queued_jobs is 780 (fails above 500); the module reported: 780 queued jobs`.
- **A `failed` or `unknown` result keeps its status.** MMWatchdog applies no
  limit to it.
- **A limit needs its measurement.** An `ok` result without a measurement
  that the check has a limit on makes the observation `unknown`, with the
  message
  `the result has no measurement named queued_jobs, which this check has a limit on`.
  A module that cannot measure at one run answers `unknown`.
- **A measurement is a plain, finite number.** The description states its
  unit. A measurement that the description does not list makes the
  observation `unknown`:
  `the module queued_jobs reported the measurement jobs, which its description does not list`.
- **Keep the name and the unit of a measurement.** MMWatchdog stores the
  readings of every scheduled run in the node's
  [measurement history](../how-to/look-back-at-measurements.md), under the
  name that the module reported. A measurement with a new name is another
  measurement in the history.
- **Every check that uses your module states an `alert_severity`,** also a
  check without a limit, because the module can answer `failed` by itself.
- **A result that breaks a rule makes the observation `unknown`,** with a
  message that names the module, such as
  `the module queued_jobs wrote a result that is not valid: a message is required`.

## A Starlark script

MMWatchdog runs a script in a process of its own, once for each run of a
check or each delivery attempt: `mmwatchdog run-starlark-module` with the
module's directory, as the module's account. That process reads the
request of the [JSON protocol](#the-json-protocol) and calls the script's
function. So a script and an executable run in the same way.

### The Starlark language

Starlark reads like Python: `def`, `if`, `for`, lists, dictionaries, and
strings with methods such as `strip()` and `split()`. The
[language specification](https://github.com/google/starlark-go/blob/master/doc/spec.md)
lists everything. These are the differences from Python that a module
writer meets:

- A script has no `while` loop and no function that calls itself, so every
  script comes to an end.
- A script is one file. It cannot load another file and has no `import`.
- An `if` or a `for` cannot stand outside a function. Outside a function, a
  script defines its functions and gives names to values, such as
  `MOST_CHARACTERS_OF_A_MESSAGE = 4000`.
- Starlark has no classes, no exceptions, and no f-strings. Format a text
  with `%`, as in `"%d queued jobs" % queued_jobs`.
- A script reaches the node only through the [primitives](#primitives). It
  cannot open a file, start a program, or use the network by itself.
- A script also has `json.encode`, `json.decode`, and `json.indent`:
  [json](#json).
- A value that the script returns, or stores with `write_state`, is `None`,
  `True`, `False`, a number, a text, a list, a tuple, or a dictionary with
  text keys. A whole number must fit in 64 bits.
- `print(...)` writes a line to the standard error of the script's process.
  `sudo mmwatchdog check-now CHECK_GROUP` shows the printed lines below the
  check, so `print` is how you look at a value while you write a script.
  MMWatchdog passes on the first 8 KiB that one run prints.

### The limits of a run

| Limit | What happens |
| --- | --- |
| The check's `time_limit_per_run`, or a destination's `time_limit_per_delivery_attempt` | Each call of a primitive that waits ends shortly before the time limit, with an `error`, so the script still decides the result: see [Primitives](#primitives). A run that takes longer all the same is ended, and the observation is `unknown`: `the module queued_jobs did not finish within 10s (time_limit_per_run), so MMWatchdog stopped the module`. |
| 10 million steps | The run ends. A check needs far fewer, and the limit keeps a script that counts without end from keeping a processor busy until the time limit. |
| `module.star` at most 1 MiB | MMWatchdog does not read a larger script. |

A script that ends with an error makes the observation `unknown`. The
message names the file and the line, such as
`the script of the module queued_jobs ended with an error: /usr/local/lib/mmwatchdog/external_modules/queued_jobs/module.star:6: key "queue_file_path" not in dict`
for a script that reads an input that the check does not state.

### Check input values before a run

A script may define `validate_inputs(inputs)`, for rules that the
description cannot state. After MMWatchdog has checked the inputs against
the description, it calls `validate_inputs` during `mmwatchdog validate`,
`mmwatchdog validate --config-only`, and reload. The function returns
`None` for valid inputs, or a text that says what to change:

```python
def validate_inputs(inputs):
    if not inputs["queue_file_path"].startswith("/"):
        return "queue_file_path: use an absolute path, such as /var/lib/myapp/pending"
    return None
```

- The text becomes a validation error at the configuration file and line.
  A text that starts with the name of an input and a colon points at that
  input. Any other text points at the module's mapping.
- `validate_inputs` checks values only. A call of a primitive inside it
  ends with the error
  `read_file cannot be called during input validation; validate_inputs checks input values only`.
  So validation also works on a machine that is not the node. `json` works.
- Only a script has `validate_inputs`. An executable checks its inputs at
  each run.

## Primitives

The **primitives** are the functions through which a script reads the
node. The core modules do their work through the same functions.

| Primitive | What it does |
| --- | --- |
| [`read_file(path)`](#read_file) | Reads a text file. |
| [`run_command(arguments, standard_input = "")`](#run_command) | Runs a program with its arguments, without a shell. |
| [`http_request(method, url, headers = {}, body = "", trusted_certificate_authority_file = "")`](#http_request) | Sends one HTTP or HTTPS request. |
| [`verify_connection(url)`](#verify_connection) | Opens and closes a connection to an HTTP or HTTPS address, and checks an HTTPS certificate, without a request. |
| [`tcp_exchange(address, text_to_send = "", read_until_text = "")`](#tcp_exchange) | Opens a TCP connection, and can send a text and read the answer. |
| [`udp_exchange(address, text_to_send)`](#udp_exchange) | Sends a text in one UDP packet and reads the packet that comes back. |
| [`read_state()` and `write_state(value)`](#state-kept-between-runs) | Keep one value from one run of a check to the next. |

- **Each primitive returns a result with named fields,** such as
  `queue_file.content`. `read_state` and `write_state` are the exception.
- **The field `error` says whether the call worked.** It is empty text when
  the call worked, and otherwise says what went wrong. A failed call does
  not stop the script, so the script decides what the failure means: a
  missing file can make one check `failed` and another `unknown`.
- **A call written wrong ends the script,** such as a call with a number
  where the primitive takes a text.
- **A call that waits ends shortly before the time limit.** A call of
  `http_request`, `verify_connection`, `tcp_exchange`, or `udp_exchange`
  without an answer by then ends with the `error`
  `MMWatchdog ended the call shortly before the time limit of the run (time_limit_per_run)`.
  A program of `run_command` that has not ended by then is ended, with the
  `error`
  `MMWatchdog ended the program shortly before the time limit of the run (time_limit_per_run)`.
  "Shortly before" is a tenth of the time limit, and at most one second:
  after 9 seconds of a 10-second limit, and after 1.8 seconds of a
  2-second limit. After that moment, every further call that waits ends
  at once with the same `error`, so a script that tries several servers in
  a loop still returns its result in time. In a destination module, the
  error names
  `the time limit of the delivery attempt (time_limit_per_delivery_attempt)`.
  The script decides whether the timeout is a failure, so a check can fail
  for a server that hangs, and its recovery action can restart the server.
- **Every primitive acts as the module's account,** with that account's
  rights and no more.

### read_file

`read_file(path)` reads the file at `path`, which holds at most 1 MiB.

| Field | Meaning |
| --- | --- |
| `content` | The content of the file. |
| `error` | Empty, or why MMWatchdog could not read the file, such as `open /var/lib/myapp/pending: no such file or directory`. |

```python
queue_file = read_file("/var/lib/myapp/pending")
if queue_file.error:
    return {"status": "unknown", "message": "MMWatchdog cannot read the queue file: " + queue_file.error}
waiting = queue_file.content.strip()
```

### run_command

`run_command(arguments, standard_input = "")` runs a program and waits for
its end. `arguments` is a list: the program and each of its arguments.
MMWatchdog runs no shell, so a `|`, a `>`, or a `*` in an argument has no
special meaning. With `standard_input`, the program gets that text on its
standard input.

| Field | Meaning |
| --- | --- |
| `exit_code` | The program's exit code, or `-1` for a program that MMWatchdog could not start. |
| `standard_output` | What the program wrote to its standard output, at most 1 MiB. |
| `standard_error` | What the program wrote to its standard error, at most 16 KiB. |
| `error` | Empty for a program that ran to its end, whatever its exit code. Otherwise why the program did not finish, such as `exec: "myapp-cli": executable file not found in $PATH`. |

```python
ran = run_command(["myapp-cli", "queue-length"])
if ran.error:
    return {"status": "unknown", "message": "MMWatchdog could not run myapp-cli: " + ran.error}
if ran.exit_code != 0:
    return {"status": "unknown", "message": "myapp-cli ended with exit code %d: %s" % (ran.exit_code, ran.standard_error.strip())}
waiting = ran.standard_output.strip()
```

- An exit code other than 0 is no error of the call: the script reads
  `exit_code` and decides.
- These are errors: a program that writes more than 1 MiB to its standard
  output, that a signal ends, or that has not ended shortly before the time
  limit.
- The program runs as the script's account, in its directory, and with its
  environment.
- When MMWatchdog ends the script at the time limit, it ends every program
  that the script started too, so no program of a run stays behind.

### http_request

`http_request(method, url, headers = {}, body = "", trusted_certificate_authority_file = "")`
sends one HTTP or HTTPS request and reads the answer.

| Field | Meaning |
| --- | --- |
| `http_status_code` | The answer's status code, such as `200`, or `0` when no answer came. |
| `headers` | The answer's headers, as a dictionary, such as `{"Content-Type": "application/json"}`. |
| `body` | The answer's body, at most 1 MiB. |
| `error` | Empty when an answer came, whatever its status code. Otherwise why none came, such as `dial tcp 127.0.0.1:8080: connect: connection refused`. |

```python
response = http_request(method = "GET", url = "http://127.0.0.1:8080/queue")
if response.error:
    return {"status": "failed", "message": "myapp did not answer: " + response.error}
if response.http_status_code != 200:
    return {"status": "failed", "message": "myapp answered HTTP %d" % response.http_status_code}
queue = json.decode(response.body, default = None)
```

- **A status code is no error.** An answer with the status code 503 has an
  empty `error`. The script reads `http_status_code` and decides.
- **A body larger than 1 MiB is an error.** The result then has the status
  code and the headers, and an empty `body`.
- **Only the time limit of the run limits the request.** There is no
  separate time limit to connect or for the TLS handshake.
- **A redirect is not followed.** The script gets the answer with the status
  code, such as 302, and its `Location` header.
- **An HTTPS certificate is always verified,** with the certificate
  authorities that the node trusts. `trusted_certificate_authority_file`
  names a file of PEM certificates, of at most 1 MiB, that MMWatchdog trusts
  for this call in place of the node's. The module's account reads that
  file.
- **A `Host` header selects the virtual host,** and for HTTPS also the name
  that MMWatchdog verifies in the certificate.
- **The `error` never holds the address of the request,** because an
  address can hold a secret, such as the token in a chat webhook's address.
  The `error` can name the host, never the path behind it.

### verify_connection

`verify_connection(url)` opens and closes a TCP connection to an `http`
address, or a TLS connection to an `https` address. For HTTPS, it checks
the certificate against the node's trusted certificate authorities and the
address's host name. It sends no HTTP request, and ignores the address's
path and query. Its result has only `error`: empty when the connection and
the certificate passed, and otherwise
`the connection or server certificate check failed`, without the address
or a credential.

Use `verify_connection` in [`verify_destination`](#check-a-destination-without-a-message)
when the service has no request that checks a credential without sending
a message. A connection that opens does not prove that the credential is
right or that the destination may post, so say that in the verification
message.

### tcp_exchange

`tcp_exchange(address, text_to_send = "", read_until_text = "")` opens a
TCP connection to `address`, written as host, colon, port, such as
`127.0.0.1:9000`.

- With neither `text_to_send` nor `read_until_text`, the call only
  establishes that the connection opens.
- With `text_to_send`, MMWatchdog sends that text.
- With `read_until_text`, MMWatchdog then reads until the answer contains
  `read_until_text`, the other side closes the connection, or MMWatchdog
  has read 64 KiB.

| Field | Meaning |
| --- | --- |
| `received` | What MMWatchdog read from the connection. |
| `error` | Empty, or why the exchange failed, such as `dial tcp 127.0.0.1:9000: connect: connection refused`. |

```python
answer = tcp_exchange(address = "127.0.0.1:9000", text_to_send = "status\n", read_until_text = "\n")
if answer.error:
    return {"status": "failed", "message": "myapp does not answer on port 9000: " + answer.error}
if answer.received.strip() != "ready":
    return {"status": "failed", "message": "myapp answered " + answer.received.strip()}
```

### udp_exchange

`udp_exchange(address, text_to_send)` sends `text_to_send` in one UDP packet
to `address`, written as host, colon, port, such as `127.0.0.1:9001`, and
waits for one packet in return.

- UDP has no connection: only an answer says that a program listens. So a
  call without an answer ends with an `error` shortly before the time limit.
- A machine that reports that no program listens on the port ends the call
  with an `error` at once.
- A host name with several addresses gets a try on each address, each with
  an equal share of the time that is left.
- MMWatchdog reads at most 64 KiB of the packet that comes back.

| Field | Meaning |
| --- | --- |
| `received` | The content of the packet that came back. |
| `error` | Empty, or why the exchange failed. |

```python
answer = udp_exchange(address = "127.0.0.1:9001", text_to_send = "status\n")
if answer.error:
    return {"status": "failed", "message": "myapp does not answer on UDP port 9001: " + answer.error}
```

### json

`json.encode(value)` writes a value as JSON text, `json.decode(text)` reads
JSON text into a value, and `json.indent(text)` writes JSON text with
indentation. An object becomes a dictionary, and a list becomes a list.

```python
queue = json.decode('{"queued_jobs": 780}')        # {"queued_jobs": 780}
text = json.encode({"title": "the queue is long"}) # '{"title":"the queue is long"}'
```

`json.decode` ends the script when the text is not JSON.
`json.decode(text, default = None)` returns `None` then, and the script
decides.

## State kept between runs

Some checks must remember something from one run to the next, such as a
count at the last run. MMWatchdog keeps one **state directory** for each
check whose module is not core. A script keeps one value there:

- `write_state(value)` stores a value for the next run of the calling
  check.
- `read_state()` returns the value that an earlier run of the calling check
  stored, or `None` when no run has stored one.

The value is a number, a text, `True`, `False`, `None`, a list, or a
dictionary with text keys, of at most 1 MiB as JSON.

The example module [`finished_jobs`](../../examples/modules/finished_jobs/module.star)
reports how many jobs the application finished since the last run. The
application writes into `/var/lib/myapp/finished` how many jobs it has
finished since it started:

```python
# mmwatchdog_external_modules/finished_jobs/module.star
def run_check(inputs, file_contents):
    counter_file = read_file(inputs["finished_jobs_counter_file_path"])
    if counter_file.error:
        return {"status": "unknown", "message": "MMWatchdog cannot read the counter file: " + counter_file.error}
    counter = counter_file.content.strip()
    if not counter.isdigit():
        return {"status": "unknown", "message": "the counter file does not hold a whole number"}
    finished_jobs_now = int(counter)

    finished_jobs_at_last_run = read_state()
    write_state(finished_jobs_now)
    if finished_jobs_at_last_run == None:
        return {"status": "unknown", "message": "this is the first run, so there is no earlier count to compare with"}

    # The application starts its count at 0 again when it restarts.
    finished = finished_jobs_now - finished_jobs_at_last_run
    if finished < 0:
        finished = finished_jobs_now
    return {
        "status": "ok",
        "message": "%d jobs finished since the last run" % finished,
        "measurements": {"jobs_finished_since_last_run": finished},
    }
```

A check with `fail_when_measurement_below: {jobs_finished_since_last_run: 1}`
and `check_every: 10m` then fails when no job finished in 10 minutes. With
4,100 finished jobs at one run and 4,130 at the next, the module reports
30. The first run has nothing to compare with and answers `unknown`.

- **The state directory** is `/var/lib/mmwatchdog-module-state/CHECK_GROUP/CHECK`
  on the node, such as
  `/var/lib/mmwatchdog-module-state/myapp_jobs/jobs_keep_finishing`. It
  belongs to the module's account, and no other account can read it. The
  node setting `module_state_dir` names another directory in place of
  `/var/lib/mmwatchdog-module-state`:
  [Configuration files](configuration-files.md).
- **Each check has its own state.** Two checks that use the same module do
  not see each other's value.
- **`mmwatchdog check-now` works on a copy.** Its run reads the value of
  the last scheduled run, and MMWatchdog discards what the run stores. So
  looking at a check never changes what the check's next scheduled run
  sees. For an executable, the copy holds the directories and regular files
  of the state directory that belong to the module's account. A symbolic
  link is not copied. MMWatchdog copies at most 64 MiB and at most 10,000
  files and directories, and with more the run of `check-now` is `unknown`
  and says so.
- **MMWatchdog removes the state with the check:** when you remove the
  check, or change it so that MMWatchdog treats it as a new check.
- **A destination module keeps no state.** `read_state` and `write_state`
  end a destination module's script with the error
  `write_state: only a check keeps state between runs`.

## Findings

Some checks watch many things at once. One run of such a check reports each
thing that is wrong as a **finding**, and each finding is its own incident,
with its own alert and its own recovery. The core module
[`every_systemd_unit`](modules/every_systemd_unit.md) works this way: each
failed systemd unit is one finding.

A module reports findings when its description states
`one_incident_for_each_finding: true`. The result then lists the findings
under `findings`, each with an `id` and a `message`. The example module
[`failed_jobs`](../../examples/modules/failed_jobs/module.star) reads a file
in which the application writes the name of each failed job, one on a
line:

```python
# mmwatchdog_external_modules/failed_jobs/module.star
def run_check(inputs, file_contents):
    failed_jobs_file = read_file(inputs["failed_jobs_file_path"])
    if failed_jobs_file.error:
        return {"status": "unknown", "message": "MMWatchdog cannot read the file of failed jobs: " + failed_jobs_file.error}

    findings = []
    for line in failed_jobs_file.content.splitlines():
        job = line.strip()
        if job:
            findings.append({"id": job, "message": "the job " + job + " failed"})

    if not findings:
        return {"status": "ok", "message": "no job failed"}
    return {
        "status": "failed",
        "message": "%d jobs failed" % len(findings),
        "findings": findings,
    }
```

With the two lines `nightly_report` and `export_invoices` in the file,
MMWatchdog opens one incident for each job. When the application takes
`nightly_report` out of the file, the `nightly_report` incident recovers,
and the `export_invoices` incident stays open.
`sudo mmwatchdog check-now CHECK_GROUP` shows each finding on a line below
the check, such as `Finding nightly_report: the job nightly_report failed`,
so you see the `id` of each incident.

| Field of a finding | Meaning |
| --- | --- |
| `id` | Required. The finding's identity: the same thing has the same `id` at every run. Text on one line, of at most 255 bytes. No two findings of one result have the same `id`. For a thing whose name can be longer, use a stable checksum as the `id` and name the thing in the message. |
| `message` | Required. What is wrong, such as `the job nightly_report failed`. |
| `recovery_message` | Optional. The text to use when the finding becomes healthy. MMWatchdog keeps it with the finding's incident, also across restarts, so a finding with a checksum `id` still names its thing in the recovery. |

- A `failed` result holds at least one finding, and an `ok` or `unknown`
  result holds none. A result holds at most 500 findings.
- Each finding follows the check's timings by itself: its incident opens
  after `alert_after_failing_for`, and recovers when no run has reported it
  for `recovered_after_healthy_for`.
- Every finding takes the check's `alert_severity`. Two levels need two
  checks.
- An `unknown` result is the check's, not a finding's. An open incident of
  a finding stays open while the module cannot check.
- A result that breaks a rule makes the observation `unknown`, with a
  message such as
  `the module failed_jobs answered failed without a finding, and its description states one_incident_for_each_finding`.

## Destination modules

A **destination module** delivers alerts to one service. An alert
destination names the module, and MMWatchdog runs the module once for each
delivery attempt. The description states `used_as: alert_destination`,
and the script defines `deliver_alert(alert, inputs, file_contents)`.
[Write your own check or destination](../how-to/write-your-own-check-or-destination.md)
builds the example module `alarm_system`.

The three arguments:

- **`alert`** is the whole alert event, as a `webhook` destination gets it:
  [Alert format](alert-format.md) lists the fields. `title` and
  `title_with_alert_severity` are always there, and some fields can be
  missing: `body`, `message`, and `evidence` for a destination that states
  `send_only_the_alert_title: true`, `body` for a notice sent without one,
  and `incident_id` and `fleet_incident_key` for an alert without an
  incident, such as a notice. Read a field that can be missing with
  `alert.get("body", "")`, or ask with `"body" in alert`.
- **`inputs`** are the destination's inputs as written.
- **`file_contents`** holds the text of each file that an input names,
  under the input's name: [Inputs that name a file](#inputs-that-name-a-file).

`title_with_alert_severity` is the title with the severity in front, such
as `Warning: app01: myapp_jobs/job_queue_is_short failed`. Send it where a
person reads the alert, such as the first line of a chat message. A service
that takes the severity as a field of its own gets `title` and
`alert_severity`.

`deliver_alert` returns a dictionary with a `status` and a `message`:

- `status` is `delivered` or `failed`, and `message` is required. For
  `failed`, `message` says why, and MMWatchdog shows it as the error of the
  delivery attempt:
  `the module alarm_system reported: the alarm system answered HTTP 503`.
- Anything else is a failed attempt too: a script that ends with an error,
  a run past the destination's `time_limit_per_delivery_attempt`, or
  another result.

Rules for a destination module:

- **A module delivers once and reports.** MMWatchdog tries a failed attempt
  again and runs the module anew each time. A module does not retry, wait,
  or keep a queue, and it keeps no state.
- **A credential stays out of the message.** Never write the content of a
  file into the `message`. `http_request` keeps the address out of its
  `error`, so `response.error` is safe to pass on. MMWatchdog also replaces
  a whole file content that a message holds with `[content of NAME]`.
- **In a fleet, every node needs the module.** A joined node that delivers
  an alert without the hub runs the module itself, with the hub's
  destination and the node's copy of the credential files. The release's
  MMX workflows install the same modules on every node.
- **Test a destination** with
  `sudo mmwatchdog send-notice --alert-severity info --title "Test of the alarm system. Nothing is wrong." --destination team_alarm_system`,
  which sends a notice through the real delivery path and prints what the
  module answered: [Commands](commands.md).

### When a delivery attempt fails

The error of a failed attempt names the module and is one line. The text
that comes from the module, such as its `message`, is cut after 300 bytes.
`sudo mmwatchdog status` and `mmwatchdog send-notice` show the error.
MMWatchdog replaces the content of each file that an input names, even a
short one, with `[content of INPUT]`, such as
`[content of api_token_file]`.

| Error | What happened |
| --- | --- |
| `the module alarm_system reported: TEXT` | The module answered `failed`. TEXT is its `message` |
| `the script of the module alarm_system ended with an error: TEXT` | The script ended with an error, such as a call of `fail()` |
| `the module alarm_system ended with exit status 3: TEXT` | An executable ended with another exit status. TEXT is the first line of its standard error |
| `the module alarm_system did not finish within 5s (time_limit_per_delivery_attempt), so MMWatchdog stopped the module` | The time limit passed. MMWatchdog ended the module and the programs that it started, unless a program left the module's process group |
| `the module alarm_system is not installed on this node` | This node has no module of that name. In a fleet, every node needs the module |
| `the module alarm_system on this node is used as a check, not as an alert destination` | This node's module of that name is a check module |
| `MMWatchdog cannot read the file of the input api_token_file of the module alarm_system: ...` | A file that an input names cannot be read, so MMWatchdog did not start the module |
| `MMWatchdog cannot use the files of the module alarm_system: ...` | The files that the inputs name are larger than 64 KiB together |
| `the module alarm_system could not run: ...` | MMWatchdog could not start the module, such as for an account that does not exist on the node |
| `the module alarm_system was ended by a signal` | Something else ended the module, such as the kernel when memory ran out |
| `the module alarm_system ended without an answer on its standard output` | An executable ended with exit status 0 and wrote no answer |
| `the module alarm_system did not answer with one JSON object of protocol, status, and message: ...` | The answer is not one JSON object |
| `the module alarm_system returned protocol 2; MMWatchdog requires protocol: 1` | The answer has another `protocol` |
| `the module alarm_system answered with the status "ok", and a destination module answers delivered or failed` | The module answered as a check module does |
| `the module alarm_system answered delivered without a message, and a message is required` | The answer has no `message` |
| `the module alarm_system wrote more than 65536 bytes to its standard output` | The answer is too large |

A module that delivered the alert but did not answer `delivered`, such as
one that ran past the time limit, gets the same alert again at the next
attempt.

### Check a destination without a message

A script may define `verify_destination(inputs, file_contents)`, which
checks the address and the credential without sending a message. It
returns a pair: `True` or `False` for whether the service accepted the
address and the credential, and a one-line text that says what was
checked, such as `(True, "verified address and credential")`. A refused
credential returns `False` with the service's reason. Never send a message
from `verify_destination`.

- MMWatchdog reads the credential files as for a delivery, so
  `file_contents` holds them, and runs the function as the destination's
  account, within `time_limit_per_delivery_attempt`.
- MMWatchdog calls it for a new or changed destination at reload, and
  every 24 hours. A failed check is tried again every minute.
- `mmwatchdog reload` prints the outcome for each new or changed
  destination, such as `Destination team_alarm_system: not tested: MMWatchdog can test the destination only by sending a message`
  for a module without `verify_destination`.
- Only a script has `verify_destination`. MMWatchdog checks an executable
  destination only by sending a message.

### A service that keeps incidents

Some services keep incidents of their own: a pager opens an incident for a
failure and resolves it for the recovery. In a fleet, one check can fail on
many nodes at once, and the service should have one incident for the
check, which ends when the last node has recovered. A module for such a
service states `service_keeps_incidents: true` in its description.
MMWatchdog then sends a destination with the module:

1. every failure, raise, and reminder, as for every destination;
2. nothing when one node recovers while another node still has an incident
   of the same check open;
3. one closing alert when the last node's incident closes: the node's own
   recovery when only that node failed, and otherwise an alert about the
   check across the fleet, without an `incident_id`, such as
   `myapp/http is ok on every node again`;
4. rarely, a failure a second time after the closing alert, when a node's
   report shows that the node still failed while its reports did not reach
   the hub. The module opens the incident again, as for any failure.

Every failure, raise, reminder, and closing alert carries
`fleet_incident_key`: 32 hexadecimal digits, the same for one check across
the fleet, or on a standalone node. So the module opens and closes the
service's incident by `fleet_incident_key`, and keeps no state:

```python
# mmwatchdog_external_modules/team_pager/module.star
def deliver_alert(alert, inputs, file_contents):
    closes = alert["type"] in ["recovered", "can_check_again", "closed_without_recovery"]
    # A notice has no key; it gets an incident of its own, by its ID.
    key = alert.get("fleet_incident_key", alert["id"])
    response = http_request(
        method = "POST",
        url = inputs["pager_url"],
        headers = {"Content-Type": "application/json"},
        body = json.encode({"incident": key, "resolve": closes, "summary": alert["title_with_alert_severity"]}),
    )
    if response.error:
        return {"status": "failed", "message": "MMWatchdog could not reach the pager: " + response.error}
    if response.http_status_code // 100 != 2:
        return {"status": "failed", "message": "the pager answered HTTP %d" % response.http_status_code}
    return {"status": "delivered", "message": "the pager took the alert"}
```

The closing alert's `alert_severities_sent_for_incident` lists the
severities that the destination got for the key, and its `alert_severity`
is the highest of them. A module that closes only the incidents that it
opened at one severity reads `alert_severities_sent_for_incident`.

## The account and the environment of a module

MMWatchdog runs a module that is not core in a process of its own, once for
each run of a check or each delivery attempt. Three **run settings**, which
a check or a destination writes beside the module's inputs, say how:

| Run setting | Meaning | When left out |
| --- | --- | --- |
| `run_as_user_account` | The account that the module runs as. | `mmwatchdog`, the unprivileged account of the checks |
| `run_from_directory` | The absolute directory that the module starts in. | `/` |
| `env` | More environment variables, as a mapping of texts. | None |

A joined node uses its fleet hub's destinations, but not their run
settings: it runs every destination module as `mmwatchdog`, from `/`, with
no added environment variables. Only the hub uses a destination's run
settings, and `mmwatchdog validate` on the hub warns about each destination
that states them. Reason: the hub must not choose the account or the
environment of programs on the nodes. A destination module that needs a
value on every node reads the value from an input or a file.

```yaml
# files/hub_and_standalone/check_groups/myapp_jobs.yml
check_groups:
  myapp_jobs:
    check_every: 30s
    time_limit_per_run: 10s
    alert_after_failing_for: 1m
    recovered_after_healthy_for: 30s
    checks:
      job_queue_is_short:
        queued_jobs:
          queue_file_path: /var/lib/myapp/pending  # an input
          run_as_user_account: myapp               # run settings
          run_from_directory: /var/lib/myapp
          env:
            MYAPP_MODE: production
        fail_when_measurement_above:
          queued_jobs: 500
        alert_severity: warn
```

- The module has the rights of its account and no more. It cannot read
  MMWatchdog's own state, the alert queue, or a credential file that only
  root reads.
- The module gets a small, fixed environment: a standard `PATH`, the
  account's `HOME` and `USER`, `LANG=C.UTF-8`, and the variables of `env`.
  It gets nothing from MMWatchdog's own environment.
  [Configuration files](configuration-files.md) lists the names that `env`
  refuses.
- An account or a directory that does not exist on the node is a
  validation error on that node.

### Inputs that name a file

An input whose name ends in `_file` names a file by its absolute path, such
as `api_token_file: /etc/mmwatchdog/secrets/alarm-system-token`. MMWatchdog
reads the file as root before each run of a check and for each delivery,
and gives the module the text in `file_contents`, under the input's name.
`inputs` keeps the path.

- **Use `_file` for a credential that the module's account must not open,**
  such as a root-owned file of mode 0600 in `/etc/mmwatchdog/secrets/`.
  MMWatchdog refuses a secret file that other accounts can read, unless the
  check or the destination allows that access:
  [Configuration files](configuration-files.md). An input whose name ends
  in `certificate_authority_file` holds a public trust bundle, so this rule
  does not apply to it, but MMWatchdog still reads the file and passes it on.
- MMWatchdog removes one final line break from the text.
- Each file holds UTF-8 text of at most 16 KiB, and the files of one check
  or one destination hold at most 64 KiB together. The limits keep the
  module's request within 1 MiB.
- A file that MMWatchdog cannot read makes a check's run `unknown`, and
  fails a delivery attempt.
- Never repeat a file's content in a result or in diagnostics. MMWatchdog
  also replaces each whole file content in the module's output with
  `[content of NAME]` before it stores the observation.
- A file input whose name does not end in `_file`, such as
  `queue_file_path`, is plain text: the module opens the path itself, with
  `read_file`, as the module's account.
- The inputs of one destination are at most 256 KiB as JSON.

## An executable in place of a script

Write an executable for a need that Starlark cannot meet, such as a library
that exists only in another language. The module's directory then holds the
file `module_executable` in place of `module.star`: a Bash script, a Go or
Rust program, or anything else that runs on the node.

```text
mmwatchdog_external_modules/
  file_age/
    module_executable           the program that MMWatchdog starts
    module_description.yml      the module's inputs, measurements, and examples
```

Choose a script when you can:

- A script needs nothing on the node but MMWatchdog. An executable needs
  whatever it uses on every node, such as an interpreter or a program that
  it calls.
- MMWatchdog sees what a script does, because a script reaches the node
  only through the primitives. MMWatchdog does not see what an executable
  does, and `mmwatchdog describe-module` and `mmwatchdog module-usage` say
  so beside each executable:
  `MMWatchdog does not see what an executable does on this node. The local node must provide everything the executable needs.`
- A compiled executable must match the node's processor.

The description, the call in the configuration, the run settings, state,
and findings work for an executable as for a script.

### The JSON protocol

MMWatchdog starts the executable once for each run of its check, and writes
one JSON object to its standard input:

```json
{"protocol": 1,
 "inputs": {"queue_file_path": "/var/lib/myapp/pending"},
 "file_contents": {},
 "state_dir": "/var/lib/mmwatchdog-module-state/myapp_jobs/job_queue_is_short",
 "mmwatchdog_ends_the_module_after_milliseconds": 9987}
```

| Request field | Meaning |
| --- | --- |
| `protocol` | Always `1`. |
| `inputs` | The check's inputs, as written. |
| `file_contents` | The text of each file that an input ending in `_file` names, under the input's name. Empty without such inputs. |
| `state_dir` | The check's [state directory](#state-kept-between-runs). The executable keeps files there. `mmwatchdog check-now` gives a copy. |
| `mmwatchdog_ends_the_module_after_milliseconds` | The time that the run has left when MMWatchdog starts the executable. MMWatchdog ends the executable then. An executable that waits for a server or a program ends its wait shortly before then, and writes its own result. |

The executable writes one JSON object to its standard output, and any
diagnostics to its standard error:

```json
{"protocol": 1,
 "status": "ok",
 "message": "780 queued jobs",
 "measurements": {"queued_jobs": 780}}
```

- The result holds what `run_check` returns, and `"protocol": 1`. A module
  that reports [findings](#findings) writes them as
  `"findings": [{"id": "nightly_report", "message": "the job nightly_report failed"}]`.
- Exit status 0 means that the executable produced a result, also a
  `failed` or an `unknown` one.
- Each of these is an execution error, which makes the observation
  `unknown`: another exit status, a start that failed, a run past the time
  limit, an end by a signal, standard output that is not exactly one JSON
  object, a key written twice, another protocol number, or more than 64
  KiB of standard output. MMWatchdog keeps what the executable wrote, for
  diagnosis. For another exit status, the message is
  `the module file_age ended with exit status 1:`, followed by the first
  line that the executable wrote to its standard error.
- MMWatchdog keeps the first 16 KiB of standard error.

A destination module that is an executable gets `alert`, `inputs`,
`file_contents`, and `mmwatchdog_ends_the_module_after_milliseconds`, and
answers `delivered` or `failed` with a message:

```json
{"protocol": 1, "status": "delivered", "message": "the alarm system accepted the alert"}
```

The example [`queued_jobs_in_go`](../../examples/modules/queued_jobs_in_go/queued_jobs_in_go.go)
is the module `queued_jobs` as a Go program, which uses Go's JSON library.
Build it for the nodes' processor, under the name `module_executable`.
`CGO_ENABLED=0` makes a program that needs no C library on the node:

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o module_executable queued_jobs_in_go.go
```

Use `GOARCH=arm64` for ARM64 nodes. A project serves nodes of one
processor kind with one build.

### The module-json helper for Bash

The `mmwatchdog` program includes a JSON helper, so an executable that is a
Bash script needs no `jq` and no Python. MMWatchdog sets the environment
variable `MMWATCHDOG_MODULE_HELPER` to the path of the `mmwatchdog`
program, for executables only.

The example [`file_age`](../../examples/modules/file_age/module_executable)
measures how old a file is, in minutes. A program that must run on time,
such as a backup, writes the file after it succeeded, and the check states
how old the file may be. Its main steps:

```bash
request=$("$MMWATCHDOG_MODULE_HELPER" module-json request --inputs file_path) || exit 1
file_path=$(printf '%s' "$request" | "$MMWATCHDOG_MODULE_HELPER" module-json get --type string /inputs/file_path) || exit 1
if [[ ! -f $file_path ]]; then
  "$MMWATCHDOG_MODULE_HELPER" module-json result --status failed \
    --message 'the file does not exist or is not a regular file'
  exit $?
fi
modified=$(stat -L -c %Y -- "$file_path") || exit 1
now=$(date +%s) || exit 1
age_minutes=$(( (now - modified) / 60 ))
"$MMWATCHDOG_MODULE_HELPER" module-json result --status ok \
  --message "the file is $age_minutes minutes old" \
  --measurement age_minutes "$age_minutes"
```

```yaml
# files/hub_and_standalone/check_groups/backup.yml
check_groups:
  backup:
    check_every: 5m
    time_limit_per_run: 10s
    alert_after_failing_for: 10m
    recovered_after_healthy_for: 5m
    checks:
      last_backup_is_fresh:
        file_age:
          file_path: /var/lib/backup/last-successful-backup
        fail_when_measurement_above:
          age_minutes: 1560                        # 26 hours
        alert_severity: warn
```

The module answers `failed` when the file does not exist. Otherwise it
answers `ok` with the file's age as `age_minutes`, and the check fails when
the file is more than 1,560 minutes old, which is 26 hours. The module
needs Bash and the programs `stat` and `date` of GNU coreutils on every
node, and the module's account needs access to the file.

| Command | What it does |
| --- | --- |
| `module-json request [--inputs NAME,...]` | Reads the request from standard input, checks its protocol and that it holds only the listed inputs, and prints it for the `get` calls that follow. |
| `module-json get [--type TYPE] POINTER` | Reads one value from the request on standard input by JSON Pointer, such as `/inputs/file_path` or `/alert/title`. `TYPE` is `string`, `integer`, `number`, `boolean`, `array`, `object`, `null`, or `json`. A missing value or a value of another type is an error. |
| `module-json result --status STATUS (--message TEXT \| --message-stdin) [--measurement NAME VALUE]... [--finding ID MESSAGE]...` | Writes a valid result. `STATUS` is `ok`, `failed`, or `unknown` for a check, and `delivered` or `failed` for a destination. |
| `module-json object [--string NAME TEXT]... [--string-stdin NAME] [--json NAME JSON]...` | Writes one JSON object with the members in the order given, for a module that sends JSON to a service. |

- **A text needs no escaping by hand.** The helper writes quotes,
  backslashes, and line breaks as JSON.
- **A `string` is printed exactly,** with no line break added. Command
  substitution, as above, is right for ordinary values. When line breaks at
  the end of a value matter, send the output straight to a file.
- **Each measurement has its own `--measurement NAME VALUE`,** and each
  finding its own `--finding ID MESSAGE`. A measurement takes no unit: the
  unit is in the description.
- **`--message-stdin` reads the message from standard input,** for a long
  message. Use exactly one of `--message` and `--message-stdin`.
- **Keep a secret out of the arguments.** Every account of a machine can
  read the arguments of a running program. `module-json object
  --string-stdin NAME` reads one text from standard input.
- **Exit status:** 0 when the command worked, 4 for wrong arguments, such
  as `--status must be ok, failed, or unknown for a check, and delivered or failed for an alert destination`,
  and 1 for data that is not valid, such as
  `value at "/inputs/file_path": expected integer`.
- **Each command reads at most 1 MiB** from standard input, which is also
  the most that MMWatchdog writes in one request.

The helper needs no running MMWatchdog and no configuration. An executable
in another language uses that language's own JSON library.

## Try a module by hand

Four commands of `mmwatchdog` try a module without a deploy and without the
running service. Each runs wherever the `mmwatchdog` program runs: on a
node, or on a Linux machine with the program of your release. They need no
`sudo`.

**Run the script once.** `mmwatchdog run-starlark-module MODULE_DIR` reads
one request from standard input, runs the script, and writes the result to
standard output:

```sh
echo '{"protocol":1,"inputs":{"queue_file_path":"/var/lib/myapp/pending"}}' |
  mmwatchdog run-starlark-module /tmp/modules-to-try/queued_jobs
```

```text
{"measurements":{"queued_jobs":12},"message":"12 queued jobs","protocol":1,"status":"ok"}
```

- The request has the form of the [JSON protocol](#the-json-protocol). A
  request that holds `alert` calls `deliver_alert`, one with
  `"verify_destination": true` calls `verify_destination`, and every other
  request calls `run_check`.
- `run-starlark-module` reads no `_file` input. Put a `file_contents`
  dictionary in the request when the module uses one.
- A script that uses `read_state` or `write_state` needs
  `"state_dir": "/a/directory/that/exists"` in the request.
- Without `mmwatchdog_ends_the_module_after_milliseconds`, the calls of the
  script have no time limit. With
  `"mmwatchdog_ends_the_module_after_milliseconds": 5000`, a call without an
  answer ends after 4.5 seconds, as in a check with a `time_limit_per_run`
  of 5 seconds.
- The script runs as your own account. On a node,
  `sudo -u myapp mmwatchdog run-starlark-module ...` runs it as the
  check's account.
- What the script prints goes to standard error.
- A script with an error ends the command with exit status 4 and one line
  that names the file and the line, such as
  `mmwatchdog: /tmp/modules-to-try/queued_jobs/module.star:6: key "queue_file_path" not in dict`.

To try an executable by hand, start `module_executable` with a request on
its standard input, and with `MMWATCHDOG_MODULE_HELPER` set to the path of
the `mmwatchdog` program when it uses the helper:

```sh
echo '{"protocol":1,"inputs":{"file_path":"/etc/hostname"},"file_contents":{},"state_dir":"/tmp"}' |
  MMWATCHDOG_MODULE_HELPER=$(command -v mmwatchdog) /tmp/modules-to-try/file_age/module_executable
```

```text
{"protocol":1,"status":"ok","message":"the file is 408 minutes old","measurements":{"age_minutes":408}}
```

**Read the description.** `mmwatchdog describe-module NAME` prints the
module's description, or says what is wrong with the description.
`--external-module-dir` names a directory that holds your modules, in
place of the node's own:

```sh
mmwatchdog describe-module queued_jobs --external-module-dir /tmp/modules-to-try
```

```text
queued_jobs: a module of the project's own (Starlark), used as a check
Counts the jobs that wait in a queue file.
Files: /tmp/modules-to-try/queued_jobs/

Inputs
  queue_file_path   string   required   The absolute path of the file that holds the number of waiting jobs.

Measurements
  queued_jobs   count   The number of jobs that wait.

Example: Fail when more than 500 jobs wait.
  queued_jobs:
    queue_file_path: /var/lib/myapp/pending
  fail_when_measurement_above:
    queued_jobs: 500
  alert_severity: warn
```

**Validate a configuration against the module.** `mmwatchdog validate`
takes `--external-module-dir`, and `--official-extra-module-dir` for the
official extra modules. On a Linux machine, in the project:

```sh
./mmwatchdog validate --config-only --config-dir files/hub_and_standalone \
  --official-extra-module-dir official_extra_modules \
  --external-module-dir mmwatchdog_external_modules
```

The command prints one line that starts with `Valid configuration:`, or the
error with its file and line.

**See which modules a configuration uses.** `mmwatchdog module-usage`
lists every module that the configuration names, with its kind, the
directory of its files, and the file and line of each check or destination
that names it. On a node:

```text
$ sudo mmwatchdog module-usage
Core modules (built into MMWatchdog)
  http                 /etc/mmwatchdog/check_groups/myapp.yml:14
  node_filesystem      /etc/mmwatchdog/check_groups/node.yml:19, /etc/mmwatchdog/check_groups/node.yml:25
  node_resource        /etc/mmwatchdog/check_groups/node.yml:10, /etc/mmwatchdog/check_groups/node.yml:13
  systemd_unit_state   /etc/mmwatchdog/check_groups/myapp.yml:9

Official extra modules (Starlark, shipped with MMWatchdog 0.1.1)
  slack                /usr/local/lib/mmwatchdog/official_extra_modules/slack/
                       /etc/mmwatchdog/alert_destinations.yml:3

The project's own modules
  alarm_system         /usr/local/lib/mmwatchdog/external_modules/alarm_system/   Starlark
                       /etc/mmwatchdog/alert_destinations.yml:8
  queued_jobs          /usr/local/lib/mmwatchdog/external_modules/queued_jobs/   Starlark
                       /etc/mmwatchdog/check_groups/myapp_jobs.yml:10
```

After a deploy, `sudo mmwatchdog check-now CHECK_GROUP` on the node runs
the checks of one check group at once and prints each result, without an
alert and without a change to an incident. [Commands](commands.md) lists
every flag of these commands.

## Limits

| Limit | Value |
| --- | --- |
| A module's name, an input's name, a measurement's name | Lower-case letters, digits, and underscores, starting with a letter, at most 64 characters |
| `module_description.yml` | 256 KiB |
| A measurement's `unit` | 64 bytes |
| `module.star` | 1 MiB |
| Steps of one script run | 10 million |
| What one script run prints | 8 KiB passed on |
| Measurements in one result | 128 |
| Findings in one result | 500 |
| A finding's `id` | 255 bytes, on one line |
| A file that a `_file` input names | 16 KiB of UTF-8 text |
| The files of one check or one destination together | 64 KiB |
| The inputs of one destination, as JSON | 256 KiB |
| One request to a module | 1 MiB |
| An executable's standard output | 64 KiB |
| A module's standard error kept | 16 KiB |
| `read_file`, `run_command` standard output, `http_request` body | 1 MiB each |
| `run_command` standard error | 16 KiB |
| `tcp_exchange` and `udp_exchange` received | 64 KiB |
| `trusted_certificate_authority_file` of `http_request` | 1 MiB |
| A value stored with `write_state`, as JSON | 1 MiB |
| The copy of a state directory for `check-now` | 64 MiB and 10,000 files and directories |
