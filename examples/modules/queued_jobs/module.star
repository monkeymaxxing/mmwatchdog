# A check module of a team's own, written as a Starlark script. It reads the
# number of waiting jobs from a file that the team's application writes.
#
# The module only measures: the check that uses it states the limit, such as
# fail_when_measurement_above: {queued_jobs: 500}.
def run_check(inputs, file_contents):
    queue_file = read_file(inputs["queue_file_path"])
    if queue_file.error:
        return {"status": "unknown", "message": "MMWatchdog cannot read the queue file: " + queue_file.error}
    waiting = queue_file.content.strip()
    if not waiting.isdigit():
        return {"status": "unknown", "message": "the queue file does not hold a whole number"}
    return {
        "status": "ok",
        "message": waiting + " queued jobs",
        "measurements": {"queued_jobs": int(waiting)},
    }
