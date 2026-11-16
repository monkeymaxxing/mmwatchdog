# pushover: the official extra module of MMWatchdog that delivers alerts to Pushover.
#
# MMWatchdog runs this module once for each alert of an alert destination that names
# it. The module sends the alert to one Pushover user or delivery group
# through Pushover's Message API: the alert's title with its severity in
# front as the message's title, and the alert's body as the message. An sos
# alert goes out at Pushover's emergency priority, which repeats the alert on
# the person's devices until somebody acknowledges it, and the one closing
# alert that MMWatchdog sends when the last node of the check recovers stops those
# repeats. README.md beside this file describes the inputs and the message,
# and module_description.yml lists the inputs for MMWatchdog.

# Pushover takes at most 250 characters in a title and 1,024 in a message.
MOST_CHARACTERS_OF_A_TITLE = 250
MOST_CHARACTERS_OF_A_MESSAGE = 1024

# Pushover's priorities: -1 shows the message without a sound, 0 with the
# sound and vibration that the person's device is set to, and 2 repeats the
# message until somebody acknowledges it.
LOW_PRIORITY = -1
NORMAL_PRIORITY = 0
EMERGENCY_PRIORITY = 2
PRIORITY_OF_ALERT_SEVERITY = {"info": LOW_PRIORITY, "warn": NORMAL_PRIORITY, "sos": EMERGENCY_PRIORITY}

# The alerts that close an incident. MMWatchdog sends a destination of this module
# one of them for each check, when the last node recovers, and it stops the
# repeats of the check's sos alerts.
TYPES_THAT_CLOSE_AN_INCIDENT = ["recovered", "can_check_again", "closed_without_recovery"]

# Pushover repeats an emergency message no more often than every 30
# seconds, and for at most 3 hours.
FEWEST_SECONDS_BETWEEN_REPEATS = 30
MOST_SECONDS_OF_REPEATS = 3 * 60 * 60

# The units of a time in MMWatchdog's notation, which is Go's, in nanoseconds, and
# the seconds of a day, MMWatchdog's one unit more.
NANOSECONDS_OF_UNIT = {
    "ns": 1,
    "us": 1000,
    "µs": 1000,
    "μs": 1000,
    "ms": 1000000,
    "s": 1000000000,
    "m": 60000000000,
    "h": 3600000000000,
}
NANOSECONDS_OF_A_SECOND = 1000000000
SECONDS_OF_A_DAY = 24 * 60 * 60

# MMWatchdog shows a module's message as one line of at most 300 bytes, and cuts a
# longer one. The words of Pushover in a message get what the module's own
# words leave, and at most 200 characters.
MOST_BYTES_OF_A_MESSAGE_OF_THE_MODULE = 300
MOST_CHARACTERS_OF_PUSHOVERS_WORDS = 200

def validate_inputs(inputs):
    if "api_server_url" in inputs and not is_web_address(inputs["api_server_url"]):
        return "api_server_url: use one web address starting with https:// or http://, such as https://api.pushover.net; this is the service's API address"
    seconds_between_repeats = seconds_of(inputs["repeat_sos_alerts_every"])
    if seconds_between_repeats == None or seconds_between_repeats < FEWEST_SECONDS_BETWEEN_REPEATS:
        return "repeat_sos_alerts_every: set repeat_sos_alerts_every to at least 30s; Pushover repeats a message no more often than every 30 seconds"
    seconds_of_repeats = seconds_of(inputs["stop_repeating_sos_alerts_after"])
    if seconds_of_repeats == None or seconds_of_repeats > MOST_SECONDS_OF_REPEATS:
        return "stop_repeating_sos_alerts_after: set stop_repeating_sos_alerts_after to at most 3h; Pushover repeats a message for at most 3 hours"
    if seconds_of_repeats < seconds_between_repeats:
        return "stop_repeating_sos_alerts_after: set stop_repeating_sos_alerts_after to at least repeat_sos_alerts_every, or Pushover never repeats an sos alert"
    return None

def verify_destination(inputs, file_contents):
    response = http_request(
        method = "POST",
        url = inputs.get("api_server_url", "https://api.pushover.net").removesuffix("/") + "/1/users/validate.json",
        headers = {"Content-Type": "application/x-www-form-urlencoded"},
        body = "token=" + file_contents["application_token_file"] + "&user=" + file_contents["user_or_group_key_file"],
    )
    if response.error:
        return (False, "MMWatchdog could not connect to Pushover: " + response.error)
    if response.http_status_code // 100 == 5:
        return (False, "Pushover verification answered HTTP %d" % response.http_status_code)
    answer = pushover_said(response)
    if response.http_status_code // 100 == 4 and answer.get("status") == 0 and (answer.get("token") == "invalid" or answer.get("user") == "invalid"):
        return (False, "Pushover does not accept the application token or user key; correct application_token_file or user_or_group_key_file")
    return (True, "Pushover accepted the verification; no message was sent")

def deliver_alert(alert, inputs, file_contents):
    application_token = file_contents["application_token_file"]
    if not is_pushover_key(application_token):
        return failed("the file that application_token_file names must hold one API token of a Pushover application, 30 letters and digits such as azGDORePK8gMaC0QOYAMyEEuzJnyUi, and nothing else")
    user_or_group_key = file_contents["user_or_group_key_file"]
    if not is_pushover_key(user_or_group_key):
        return failed("the file that user_or_group_key_file names must hold one Pushover user key or group key, 30 letters and digits such as uQiRzpo4DXghDmr9QzzfQu27cmVRsG, and nothing else")
    api_server_url = inputs.get("api_server_url", "https://api.pushover.net")
    api_server_url = api_server_url.removesuffix("/")

    seconds_between_repeats = seconds_of(inputs["repeat_sos_alerts_every"])
    seconds_of_repeats = seconds_of(inputs["stop_repeating_sos_alerts_after"])

    # An sos alert is an emergency: Pushover repeats it until somebody
    # acknowledges it. The alert that closes an incident carries the highest
    # severity of what it closes, and is good news, so it goes at normal
    # priority.
    closes_an_incident = alert.get("type", "") in TYPES_THAT_CLOSE_AN_INCIDENT
    priority = PRIORITY_OF_ALERT_SEVERITY.get(alert.get("alert_severity", ""), NORMAL_PRIORITY)
    if closes_an_incident and priority == EMERGENCY_PRIORITY:
        priority = NORMAL_PRIORITY

    # Every emergency message about one check carries the tag of the check's
    # fleet incident key, on every node of a fleet, also a message in which
    # the MMWatchdog fleet hub grouped several nodes. MMWatchdog sends a destination of this
    # module one closing alert for each key, when the last node recovers,
    # because module_description.yml says service_keeps_incidents: true.
    # When an sos alert of the key went to this destination, that alert
    # first stops the repeats of every message with the tag, so the module
    # keeps no receipt of Pushover. The closing alert lists in
    # alert_severities_sent_for_incident the severities that went to this
    # destination alone. The stop reaches every destination with the same
    # application token, so a destination that got no sos alert of the key
    # stops nothing: another destination with the token can still repeat an
    # sos alert of a node that fails. A reminder goes to one destination, so
    # it stops nothing either. A notice, a reboot notice, and a maintenance
    # alert have no key, and so no tag.
    tag = tag_of_fleet_incident(alert.get("fleet_incident_key", ""))
    sent_to_this_destination = alert.get("alert_severities_sent_for_incident") or [alert.get("alert_severity", "")]
    if tag != "" and closes_an_incident and "sos" in sent_to_this_destination:
        stop_repeats(api_server_url, application_token, tag)

    # The title is the alert's title with its severity in front, such as
    # "SOS: app-1: myapp/http failed", and the message is the alert's body.
    # MMWatchdog writes both. The body goes in Pushover's monospace font, which shows
    # the output of a check as it is: Pushover reads no formatting in such a
    # message. Pushover takes no message without text, so an alert without a
    # body, such as one for a destination that takes only the title, sends
    # its title as the message, and Pushover shows the name of the
    # application above it.
    message = {"token": application_token, "user": user_or_group_key, "priority": priority}
    body = alert.get("body", "")
    if body != "":
        message["title"] = cut(alert["title_with_alert_severity"], MOST_CHARACTERS_OF_A_TITLE)
        message["message"] = cut(body, MOST_CHARACTERS_OF_A_MESSAGE)
        message["monospace"] = 1
    else:
        message["message"] = cut(alert["title_with_alert_severity"], MOST_CHARACTERS_OF_A_MESSAGE)
    if priority == EMERGENCY_PRIORITY:
        message["retry"] = seconds_between_repeats
        message["expire"] = seconds_of_repeats
        if tag != "":
            message["tags"] = tag

    # The token and the key are in the body of the request, never in its
    # address or in a message of the module.
    response = http_request(
        method = "POST",
        url = api_server_url + "/1/messages.json",
        headers = {"Content-Type": "application/json"},
        body = json.encode(message),
    )
    if response.error:
        return failed_with_words("MMWatchdog could not send the message to Pushover: ", response.error, "")

    # Pushover answers a message that it took with the status code 200 and
    # "status": 1. Every other answer is a failure, and MMWatchdog tries again later.
    answer = pushover_said(response)
    if response.http_status_code == 200 and answer.get("status") == 1:
        return {"status": "delivered", "message": "Pushover accepted the message"}
    return failure_of(response, answer)

def stop_repeats(api_server_url, application_token, tag):
    """Asks Pushover to stop repeating every emergency message with the tag.

    What Pushover answers never decides the delivery: the message that
    follows decides it, so a recovery never waits behind repeats that
    Pushover cannot stop, and a person who is still alerted acknowledges the
    alert. The request is a form, the format that Pushover documents for
    every call: the token holds only letters and digits, and the tag only
    letters, digits, and underscores, so neither needs encoding.
    """
    http_request(
        method = "POST",
        url = api_server_url + "/1/receipts/cancel_by_tag/" + tag + ".json",
        headers = {"Content-Type": "application/x-www-form-urlencoded"},
        body = "token=" + application_token,
    )

def failed(message):
    return {"status": "failed", "message": message}

def failed_with_words(before, words, after):
    """Returns a failure with words of Pushover or of the node between the module's own words, on one line that MMWatchdog shows whole."""
    most_bytes = MOST_BYTES_OF_A_MESSAGE_OF_THE_MODULE - len(before) - len(after)
    return failed(before + fitted(words, most_bytes) + after)

def failure_of(response, answer):
    """Says what an answer of Pushover that is a failure means, and what to do about it."""
    http_status_code = response.http_status_code
    errors = errors_of(answer)
    pushover_answered = "Pushover answered %d: " % http_status_code

    # Pushover answers 429 at the monthly limit of the account, and to a
    # node that it blocks for a while after many requests that it refused.
    # Its answer at the monthly limit names the token as invalid, so the
    # advice about a wrong token does not apply.
    if http_status_code == 429:
        words = errors or one_line(first_line_of(response.body))
        if "monthly limit" in words:
            advice = ". The account has used up its messages of this month. Pushover takes messages again on the 1st of next month, or after more are bought."
        else:
            advice = ". Pushover takes no messages for now: the account used up its messages of this month, or Pushover blocks this node after many refused requests."
        return failed_with_words(pushover_answered, words or "no text", advice)

    # Something other than Pushover can answer, such as a proxy, and
    # Pushover itself answers a node that it is about to block with a text
    # that says why.
    if errors == "":
        before = "the API server answered %d without Pushover's JSON" % http_status_code
        after = ". Pushover may have a temporary problem; if it lasts, check api_server_url and what stands between this node and that server."
        words = one_line(first_line_of(response.body))
        if words == "":
            return failed(before + after)
        return failed_with_words(before + ": ", words, after)
    return failed_with_words(pushover_answered, errors, "." + advice_for(answer))

def advice_for(answer):
    """Says what to do about a refusal of Pushover, by the input that Pushover names as invalid."""
    if answer.get("token") == "invalid":
        return " Pushover does not know this application token. Copy it from pushover.net into the file that application_token_file names."
    if answer.get("user") == "invalid":
        return " Pushover does not know this user or group key, or its user has no active device. Copy the key from pushover.net into the file that user_or_group_key_file names."
    return ""

def pushover_said(response):
    """Returns Pushover's answer, which is a JSON object, or an empty dictionary for another answer."""
    answer = json.decode(response.body, default = None)
    if type(answer) != "dict":
        return {}
    return answer

def errors_of(answer):
    """Returns the texts of Pushover's list errors on one line, or "" for an answer without such a list."""
    errors = answer.get("errors")
    if type(errors) != "list":
        return ""
    texts = [one_line(error) for error in errors if type(error) == "string"]
    return "; ".join([text for text in texts if text != ""])

def first_line_of(text):
    """Returns the first line of a text."""
    return text.split("\n")[0]

def one_line(text):
    """Returns a text as MMWatchdog shows it in one line: every run of white space as one space, no white space at the ends, and no control character."""
    kept = []
    for character in characters(text):
        if character.isspace():
            kept.append(" ")
        elif ord(character) >= 32 and not (ord(character) >= 127 and ord(character) < 160):
            kept.append(character)
    return " ".join("".join(kept).split())

def fitted(words, most_bytes):
    """Returns words on one line, cut to 200 characters and to a number of bytes, with "…" at a cut."""
    all_characters = characters(one_line(words))
    whole = "".join(all_characters)
    if len(all_characters) <= MOST_CHARACTERS_OF_PUSHOVERS_WORDS and len(whole) <= most_bytes:
        return whole
    kept = ""
    for character in all_characters[:MOST_CHARACTERS_OF_PUSHOVERS_WORDS - 1]:
        if len(kept) + len(character) + len("…") > most_bytes:
            break
        kept += character
    return kept.rstrip() + "…"

def tag_of_fleet_incident(fleet_incident_key):
    """Returns the tag of the emergency messages about one check, such as mmwatchdog_fleet_5925e47138c3e039dd7d115ca91fefaf, or "" for an alert without a fleet incident key.

    MMWatchdog writes the fleet incident key as 32 hexadecimal digits, the same for
    every alert about one check, or about one finding of a check, on every
    node and in every way an alert goes. So the tag holds only letters,
    digits, and underscores, which Pushover takes in a tag and in the address
    of a stop, whatever the names of the nodes and the checks are.
    """
    if fleet_incident_key == "" or len(fleet_incident_key) > 64:
        return ""
    for character in characters(fleet_incident_key):
        if character not in "0123456789abcdef":
            return ""
    return "mmwatchdog_fleet_" + fleet_incident_key

def seconds_of(duration):
    """Returns the whole seconds of a time in MMWatchdog's notation, or None for a text that is no such time.

    MMWatchdog reads a time as Go does, such as 90s, 1.5h, 100ms, or 1h30m, and
    also takes a whole number of days, such as 7d (ParseDuration in MMWatchdog).
    Pushover takes whole seconds, so the module drops a fraction of a
    second.
    """
    if duration.endswith("d"):
        days = duration.removesuffix("d")
        if days == "" or not is_digits(days):
            return None
        return int(days) * SECONDS_OF_A_DAY
    text = duration
    negative = text.startswith("-")
    if text.startswith("-") or text.startswith("+"):
        text = text[1:]
    if text == "0":
        return 0

    # A time is one or more numbers, each with its unit, such as 1h30m. A
    # number can have a fraction, such as 1.5h.
    parts = []
    whole, fraction, has_point, unit = "", "", False, ""
    for character in characters(text):
        if character in "0123456789.":
            if unit != "":
                parts.append((whole, fraction, unit))
                whole, fraction, has_point, unit = "", "", False, ""
            if character == ".":
                if has_point:
                    return None
                has_point = True
            elif has_point:
                fraction += character
            else:
                whole += character
        else:
            unit += character
    parts.append((whole, fraction, unit))

    nanoseconds = 0
    for whole, fraction, unit in parts:
        if (whole == "" and fraction == "") or unit not in NANOSECONDS_OF_UNIT:
            return None
        nanoseconds += int(whole or "0") * NANOSECONDS_OF_UNIT[unit]
        if fraction != "":
            scale = 1
            for _ in characters(fraction):
                scale *= 10
            nanoseconds += int(fraction) * NANOSECONDS_OF_UNIT[unit] // scale
    if negative and nanoseconds > 0:
        return None
    return nanoseconds // NANOSECONDS_OF_A_SECOND

def is_digits(text):
    """Reports a text of ASCII digits only."""
    for character in characters(text):
        if character not in "0123456789":
            return False
    return True

def cut(text, most_characters):
    """Cuts a text to a number of characters, and marks the cut with "…"."""
    all_characters = characters(text)
    if len(all_characters) <= most_characters:
        return text
    return "".join(all_characters[:most_characters - 1]) + "…"

def characters(text):
    """Returns the characters of a text as a list, so that a text can be counted and cut by characters and not by bytes."""
    return list(text.codepoints())

def is_pushover_key(text):
    """Reports a text of the form of Pushover's application tokens, user keys, and group keys: 30 letters and digits of ASCII."""
    if len(text) != 30:
        return False
    for character in characters(text):
        if not (character.isalnum() and ord(character) < 128):
            return False
    return True

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
