# A destination module of a team's own, written as a Starlark script. It
# delivers each alert to the team's alarm system: a program of their own that
# takes alerts as JSON over HTTP and asks for a token.
#
# MMWatchdog calls deliver_alert once for each alert, with the alert, the
# destination's inputs, and the content of the file that the input
# api_token_file names. MMWatchdog reads that file, so the module never opens it.
def deliver_alert(alert, inputs, file_contents):
    alert_for_the_alarm_system = {
        "title": alert["title"],
        "alert_severity": alert["alert_severity"],
        "type": alert["type"],
    }

    # A notice has no incident, and an alert for a destination that takes only
    # the title has no body.
    for name in ["incident_id", "body"]:
        if name in alert:
            alert_for_the_alarm_system[name] = alert[name]

    response = http_request(
        method = "POST",
        url = inputs["alarm_system_url"],
        headers = {
            "Content-Type": "application/json",
            "Authorization": "Bearer " + file_contents["api_token_file"],
        },
        body = json.encode(alert_for_the_alarm_system),
    )

    # MMWatchdog never puts the address or a header into response.error, so the
    # message cannot repeat the token.
    if response.error:
        return {"status": "failed", "message": "MMWatchdog could not reach the alarm system: " + response.error}
    if response.http_status_code // 100 != 2:
        return {"status": "failed", "message": "the alarm system answered HTTP %d" % response.http_status_code}
    return {"status": "delivered", "message": "the alarm system accepted the alert (HTTP %d)" % response.http_status_code}
