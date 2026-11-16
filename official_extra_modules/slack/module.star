# slack: the official extra module of MMWatchdog that delivers alerts to Slack.
#
# MMWatchdog runs this module once for each alert of an alert destination that names
# it. The module posts the alert to a Slack incoming webhook: the alert's
# title with its severity in front, and below it the alert's body in a code
# block. It works for every service that takes Slack's incoming webhook
# format. README.md beside this file describes the inputs and the message,
# and module_description.yml lists the inputs for MMWatchdog.

# Slack asks for at most 4,000 characters in a message, counted as Slack
# gets them.
MOST_CHARACTERS_OF_A_MESSAGE = 4000

THREE_BACKTICKS = "```"

def verify_destination(inputs, file_contents):
    webhook_url = file_contents["webhook_url_file"]
    host = webhook_url.partition("://")[2].partition("/")[0].partition("?")[0].partition("#")[0].lower()
    # Other services accept Slack's message format but may post malformed
    # requests. Only Slack itself gets the non-JSON verification body.
    if host.partition(":")[0] != "hooks.slack.com":
        connection = verify_connection(webhook_url)
        if connection.error:
            return (False, "MMWatchdog could not connect to the webhook: " + without_webhook_path(connection.error, webhook_url))
        return (True, "The webhook connection and certificate were verified; its address and credential were not checked")
    response = http_request(method = "POST", url = webhook_url, headers = {"Content-Type": "application/json"}, body = "mmwatchdog verify")
    if response.error:
        return (False, "MMWatchdog could not connect to Slack: " + without_webhook_path(response.error, webhook_url))
    if response.http_status_code // 100 == 5:
        return (False, "Slack verification answered HTTP %d" % response.http_status_code)
    if response.http_status_code == 404 and response.body.strip() == "no_team":
        return (False, "Slack does not know this workspace; replace the address in webhook_url_file")
    if response.http_status_code in [400, 403, 404] and response.body.strip() in ["no_service", "invalid_token", "no_active_hooks", "team_disabled"]:
        return (False, "Slack does not accept this webhook; replace the address in webhook_url_file")
    return (True, "Slack accepted the verification; no message was sent")

def deliver_alert(alert, inputs, file_contents):
    webhook_url = file_contents["webhook_url_file"]
    if not is_web_address(webhook_url):
        return failed("the file that webhook_url_file names must hold one web address that starts with https:// or http://, and nothing else")

    # The first line of the message is the alert's title with its severity
    # in front, such as "SOS: app-1: myapp/http failed". MMWatchdog writes that line.
    # With the input mention, the mention starts the line. The mention keeps
    # its < and >: it is Slack's own notation.
    first_line = as_slack_text(alert["title_with_alert_severity"])
    if "mention" in inputs:
        first_line = inputs["mention"] + " " + first_line
    text = first_line

    # Below the first line comes the alert's body, in a code block: Slack
    # shows the text between two lines of three backticks as it is, so *, _,
    # and ~ in the output of a check stay those characters and do not turn
    # into bold or italic text. An alert for a destination that takes only
    # the title has no body.
    if "body" in alert:
        # A longer body is cut, and "…" marks the cut. The first line and the
        # two lines of backticks, which take 9 characters with their line
        # breaks, always arrive.
        most_characters_of_body = max(1, MOST_CHARACTERS_OF_A_MESSAGE - len(characters(first_line)) - 9)
        body = cut_slack_text(as_slack_text(alert["body"]), most_characters_of_body)
        text += "\n" + THREE_BACKTICKS + "\n" + body + "\n" + THREE_BACKTICKS

    # The address of the webhook is the secret. The primitive http_request
    # never writes an address into an error.
    response = http_request(
        method = "POST",
        url = webhook_url,
        headers = {"Content-Type": "application/json"},
        body = json.encode({"text": text}),
    )
    if response.error:
        return failed("MMWatchdog could not send the message to Slack: " + without_webhook_path(response.error, webhook_url))

    # Slack acknowledges a delivery with the text "ok". A proxy's login
    # page can also answer 200 and must not acknowledge a lost alert.
    if response.http_status_code // 100 == 2 and response.body.strip() == "ok":
        return {"status": "delivered", "message": "Slack accepted the message"}
    slack_said = "".join(characters(without_webhook_path(response.body.split("\n")[0], webhook_url))[:200])
    return failed("Slack answered %d: %s.%s" % (response.http_status_code, slack_said or "no text", advice_for(response.http_status_code, slack_said)))

def without_webhook_path(text, webhook_url):
    """Hides the webhook address, path, and credential segments before a diagnostic is cut."""
    path = webhook_url.partition("://")[2].partition("/")[2]
    text = text.replace(webhook_url, "[address of the webhook]")
    if path:
        text = text.replace("/" + path, "[path of the webhook]").replace(path, "[path of the webhook]")
        for segment in path.partition("?")[0].split("/"):
            if segment and segment != "services":
                text = text.replace(segment, "[part of the webhook's path]")
    return text

def failed(message):
    return {"status": "failed", "message": message}

def advice_for(http_status_code, slack_said):
    """Says what to do about an answer of Slack that is a failure."""
    if http_status_code == 429:
        return " Slack takes no more messages from this webhook for a short time."
    if slack_said in ["no_service", "no_active_hooks", "no_service_id", "no_team", "invalid_token", "team_disabled"]:
        return " The address of the webhook does not work any more. Make a new incoming webhook in Slack and write its address into the file that webhook_url_file names."
    if slack_said in ["channel_not_found", "channel_is_archived"]:
        return " The channel of the webhook does not exist any more or is archived. Make a new incoming webhook for another channel."
    if slack_said in ["action_prohibited", "posting_to_general_channel_denied"]:
        return " An administrator of the Slack workspace does not let this webhook post to its channel."
    return ""

def characters(text):
    """Returns the characters of a text as a list, so that a text can be counted and cut by characters and not by bytes."""
    return list(text.codepoints())

def is_web_address(text):
    """Reports one web address that starts with https:// or http://, without a space, a control character, a quote, or a backslash."""
    after_the_scheme = ""
    for scheme in ["https://", "http://"]:
        if text.startswith(scheme):
            after_the_scheme = text[len(scheme):]
    if after_the_scheme == "":
        return False
    for character in characters(text):
        if ord(character) <= 32 or ord(character) == 127 or character in ['"', "\\"]:
            return False
    return True

def as_slack_text(text):
    """Returns a text of the alert as Slack takes it.

    Three backticks in a row start or end a code block in Slack, so the text
    gets them as three single quotes. Slack reads &, <, and > as control
    characters, so the text gets them as &amp;, &lt;, and &gt;, which Slack
    shows as the three characters.
    """
    return text.replace(THREE_BACKTICKS, "'''").replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")

def cut_slack_text(text, most_characters):
    """Cuts a text that as_slack_text returned to a number of characters, and marks the cut with "…"."""
    all_characters = characters(text)
    if len(all_characters) <= most_characters:
        return text
    kept = "".join(all_characters[:most_characters - 1])

    # The cut can fall inside &amp;, &lt;, or &gt;. Every & in the text
    # starts one of them, so a last & without a ; after it starts a cut one,
    # which goes.
    last_ampersand = kept.rfind("&")
    if last_ampersand >= 0 and ";" not in kept[last_ampersand:]:
        kept = kept[:last_ampersand]
    return kept + "…"
