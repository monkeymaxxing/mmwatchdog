# zfs_pool: the official extra module of MMWatchdog that checks one ZFS pool.
#
# MMWatchdog runs this module once for each run of a check that names it. The module
# asks the zpool program about the pool: whether the pool is healthy, how
# full it is, how fragmented its free space is, and how long ago it was last
# scrubbed. README.md beside this file describes the inputs and the
# messages, and module_description.yml lists the inputs and the
# measurements for MMWatchdog.

# The headings of "zpool status" that follow the one that says what is
# wrong.
HEADINGS_AFTER_STATUS = ["action:", "see:", "scan:", "config:", "errors:", "remove:", "checkpoint:"]

SECONDS_IN_A_DAY = 86400

def run_check(inputs, file_contents):
    pool = inputs["pool_name"]

    # -H writes one line without a heading, with a tab between the columns,
    # and -p writes plain numbers: 42 and not 42%.
    listed = run_command(["zpool", "list", "-H", "-p", "-o", "name,health,capacity,fragmentation", pool])
    if listed.error:
        return unknown("MMWatchdog cannot run the zpool program: " + listed.error)
    if listed.exit_code != 0:
        said = first_line(listed.standard_error)
        if "no such pool" in said:
            # A pool that this node did not import is not there for the
            # programs that use it.
            return failed("this node has no ZFS pool named " + pool)
        return unknown("zpool could not list the pool %s: %s" % (pool, said or "exit code %d" % listed.exit_code))
    columns = listed.standard_output.strip().split("\t")
    if len(columns) != 4:
        return unknown("zpool gave an answer about the pool %s that the module does not understand: %s" % (pool, first_line(listed.standard_output)))
    health = columns[1]

    # A pool without a number for one of the two has no such measurement.
    measurements = {}
    if columns[2].isdigit():
        measurements["space_used_percent"] = int(columns[2])
    if columns[3].isdigit():
        measurements["free_space_fragmentation_percent"] = int(columns[3])

    # "zpool status" says what is wrong with a pool, and when it was last
    # scrubbed.
    status = run_command(["zpool", "status", pool])
    if status.error or status.exit_code != 0:
        whole_status = ""
        about_the_scrub = "; MMWatchdog could not read when it was last scrubbed: zpool status ended with " + (status.error or first_line(status.standard_error) or "exit code %d" % status.exit_code)
    else:
        whole_status = status.standard_output
        days, about_the_scrub = days_since_last_scrub(pool, whole_status)
        if days != None:
            measurements["days_since_last_scrub"] = days

    if health != "ONLINE":
        return {"status": "failed", "message": "the ZFS pool %s is %s%s" % (pool, health, what_zpool_says(whole_status)), "measurements": measurements}

    # A pool can be ONLINE and still have a problem, such as a device with
    # errors. "zpool status -x" says "is healthy" for a pool without one.
    checked = run_command(["zpool", "status", "-x", pool])
    if checked.error or checked.exit_code != 0:
        said = checked.error or first_line(checked.standard_error) or "exit code %d" % checked.exit_code
        return {"status": "unknown", "message": "the ZFS pool %s is ONLINE, and zpool could not say whether it has a problem: %s" % (pool, said), "measurements": measurements}
    if "is healthy" not in checked.standard_output:
        return {"status": "failed", "message": "the ZFS pool %s is ONLINE and has a problem%s" % (pool, what_zpool_says(whole_status)), "measurements": measurements}
    return {"status": "ok", "message": "the ZFS pool %s is ONLINE and healthy%s" % (pool, about_the_scrub), "measurements": measurements}

def days_since_last_scrub(pool, whole_status):
    """Counts from the last completed scrub seen by MMWatchdog, or from pool creation."""
    stored = read_state()
    completed_date = stored["last_completed_scrub_date"] if stored != None and stored["pool_name"] == pool else ""
    completed = [line.strip() for line in whole_status.split("\n") if line.strip().startswith("scan: scrub repaired ")]
    if completed:
        date = zpool_date(completed[0])
        converted, problem = seconds_since_1970([date])
        about = "; its last scrub ended on " + date
        if problem:
            return None, about + "; " + problem
        write_state({"pool_name": pool, "last_completed_scrub_date": date})
        return (converted[0] - converted[1]) / SECONDS_IN_A_DAY, about
    if completed_date:
        converted, problem = seconds_since_1970([completed_date])
        about = "; its last completed scrub observed by MMWatchdog ended on " + completed_date
        if problem:
            return None, about + "; " + problem
        return (converted[0] - converted[1]) / SECONDS_IN_A_DAY, about

    # A canceled scrub or a resilver says nothing about when the whole
    # pool was last scrubbed. Count from creation until MMWatchdog sees a completed
    # scrub, so starting another scan cannot make an overdue pool healthy.
    created = run_command(["zfs", "get", "-H", "-p", "-o", "value", "creation", pool])
    about = "; MMWatchdog has not observed a completed scrub, so days_since_last_scrub counts from the pool's creation"
    if created.error or created.exit_code != 0 or not created.standard_output.strip().isdigit():
        return None, "; MMWatchdog has not observed a completed scrub and could not read when the pool was created: zfs get creation ended with " + (created.error or first_line(created.standard_error) or first_line(created.standard_output) or "exit code %d" % created.exit_code)
    now, problem = seconds_since_1970([])
    if problem:
        return None, about + "; " + problem
    return (now[0] - int(created.standard_output.strip())) / SECONDS_IN_A_DAY, about

def seconds_since_1970(dates):
    """Returns the current time and each date that zpool wrote, in seconds since 1970, and a text that says why it cannot, or "".

    The date program reads the dates in the node's own time zone, in which
    zpool writes them.
    """
    converted = run_command(["date", "--file=-", "+%s"], standard_input = "\n".join(["now"] + dates) + "\n")
    numbers = converted.standard_output.split()
    if converted.error or converted.exit_code != 0 or len(numbers) != 1 + len(dates) or len([number for number in numbers if not number.lstrip("-").isdigit()]) > 0:
        return [], "MMWatchdog could not read the date %s with the date program: %s" % (", ".join(dates) or "now", converted.error or first_line(converted.standard_error) or "exit code %d" % converted.exit_code)
    return [int(number) for number in numbers], ""

def zpool_date(line):
    """The date at the end of a scan line: zpool writes it with C's ctime as the last five words, such as "Fri Oct  2 08:51:09 2026", with two spaces before a day of the month below 10. The date has one space between the words."""
    return " ".join(line.split()[-5:])

def what_zpool_says(whole_status):
    """Returns what "zpool status" says is wrong with the pool, for a message: the text after "status:", which zpool writes on several lines."""
    said = []
    for line in whole_status.split("\n"):
        line = line.strip()
        if line.startswith("status:"):
            said.append(line[len("status:"):].strip())
        elif said and (not line or len([heading for heading in HEADINGS_AFTER_STATUS if line.startswith(heading)]) > 0):
            break
        elif said:
            said.append(line)
    return ": " + " ".join(said) if said else ""

def first_line(text):
    return text.strip().split("\n")[0]

def failed(message):
    return {"status": "failed", "message": message}

def unknown(message):
    return {"status": "unknown", "message": message}
