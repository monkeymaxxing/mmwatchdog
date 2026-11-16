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
