# apache_status: the official extra module of MMWatchdog that reads the status page
# of an Apache web server.
#
# MMWatchdog runs this module once for each run of a check that names it. The module
# requests the page that Apache's mod_status serves, in the form for
# programs, and reports how many of Apache's workers are busy. README.md
# beside this file describes the inputs and the messages, and
# module_description.yml lists the inputs and the measurements for MMWatchdog.

def run_check(inputs, file_contents):
    response = http_request(method = "GET", url = inputs["url"])
    if response.error:
        return failed("Apache did not answer the request for its status page: " + response.error)
    if response.http_status_code != 200:
        return failed("Apache answered the request for its status page with HTTP %d; the page of mod_status answers with 200" % response.http_status_code)

    # The page for programs has one line for each value, with the name, a
    # colon, and the value.
    values = {}
    for line in response.body.split("\n"):
        name, colon, value = line.partition(":")
        if colon:
            values[name.strip()] = value.strip()
    if not values.get("BusyWorkers", "").isdigit() or not values.get("IdleWorkers", "").isdigit():
        return unknown("the answer is not the status page of Apache in the form for programs: the address must be the one of mod_status and end with ?auto")
    busy, idle = int(values["BusyWorkers"]), int(values["IdleWorkers"])

    # The scoreboard has one character for each worker that Apache can
    # start, so its length is the most requests that Apache serves at once.
    most = len(values.get("Scoreboard", "")) or busy + idle
    measurements = {"busy_workers": busy, "idle_workers": idle}
    if most > 0:
        measurements["busy_workers_percent"] = 100.0 * busy / most
    return {
        "status": "ok",
        "message": "Apache answers; %d of at most %d workers are busy" % (busy, most),
        "measurements": measurements,
    }

def failed(message):
    return {"status": "failed", "message": message}

def unknown(message):
    return {"status": "unknown", "message": message}
