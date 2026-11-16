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
