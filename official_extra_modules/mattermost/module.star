# mattermost: the official extra module of MMWatchdog that delivers alerts to
# Mattermost.
#
# MMWatchdog runs this module once for each alert of an alert destination that names
# it. The module posts the alert to a Mattermost incoming webhook: the alert's
# title with its severity in front, and below it the alert's body in a code
# block. README.md beside this file describes the inputs and the message,
# and module_description.yml lists the inputs for MMWatchdog.

# Mattermost takes at most 16,383 characters in a post, counted as Unicode
# characters. It splits a longer text of a webhook into several posts, which
# would break the code block apart, so the module sends no more.
MOST_CHARACTERS_OF_A_MESSAGE = 16383

# An invisible character that ends a word for Mattermost. Placed after an @
# of the alert's title, it keeps the @ from mentioning anybody.
ZERO_WIDTH_SPACE = "\u200b"

# What the module writes in Mattermost's answer in place of the webhook's key.
IN_PLACE_OF_THE_KEY = "[the webhook's key]"

# MMWatchdog keeps the first 300 bytes of the message of a module. The words of a
# server in a message get only the bytes that are left, so that the advice
# after them always arrives whole.
MOST_BYTES_OF_A_RESULT_MESSAGE = 300

def verify_destination(inputs, file_contents):
    # This invalid channel cannot receive a post. A 404 proves the webhook
    # lookup succeeded; an invalid webhook is refused first with 400.
    response = http_request(
        method = "POST",
        url = file_contents["webhook_url_file"],
        headers = {"Content-Type": "application/json"},
        body = json.encode({"text": "MMWatchdog checks this webhook. Mattermost does not post this text.", "channel": "mmwatchdog!verify"}),
    )
    if response.error:
        return (False, "MMWatchdog could not connect to Mattermost: " + response.error)
    if response.http_status_code // 100 == 5:
        return (False, "Mattermost verification answered HTTP %d" % response.http_status_code)
    if response.http_status_code == 400:
        return (False, "Mattermost does not accept this webhook; replace the address in webhook_url_file")
    return (True, "Mattermost accepted the verification; no message was sent")

def deliver_alert(alert, inputs, file_contents):
    webhook_url = file_contents["webhook_url_file"]
    key = key_of_webhook(webhook_url)
    if key == "":
        return failed("the file that webhook_url_file names must hold one address of a Mattermost incoming webhook, such as https://chat.example.com/hooks/9xuqwrwgstrb3mzrxb83nb357a, and nothing else")

    # The first line of the message is the alert's title with its severity
    # in front, such as "SOS: app-1: myapp/http failed". MMWatchdog writes that line.
    # With the input mention, the mention starts the line, as it is: it is
    # Mattermost's own notation.
    first_line = as_mattermost_line(alert["title_with_alert_severity"])
    if "mention" in inputs:
        first_line = inputs["mention"] + " " + first_line
    text = first_line

    # Below the first line comes the alert's body, in a code block, which
    # Mattermost shows as it is: no character of the body formats, links, or
    # mentions anybody. The fence is longer than every row of backticks in
    # the body, so no line of the body can end the code block. An alert for
    # a destination that takes only the title has no body.
    body = alert.get("body", "")
    if body != "":
        fence = "`" * max(3, longest_row_of_backticks(body) + 1)

        # A longer body is cut, and "…" marks the cut. The first line and the
        # two fences, with their three line breaks, always arrive.
        most_characters_of_body = max(1, MOST_CHARACTERS_OF_A_MESSAGE - len(characters(first_line)) - 2 * len(fence) - 3)
        text += "\n" + fence + "\n" + cut(body, most_characters_of_body) + "\n" + fence

    # The address of the webhook is the secret. The primitive http_request
    # never writes an address into an error.
    response = http_request(
        method = "POST",
        url = webhook_url,
        headers = {"Content-Type": "application/json"},
        body = json.encode({"text": text}),
    )
    if response.error:
        return failed(with_words("MMWatchdog could not send the message to Mattermost: ", response.error, ""))

    # Mattermost answers an accepted message with the status code 200 and
    # the text "ok". Every other answer is a failure, and MMWatchdog tries again
    # later.
    if response.http_status_code == 200 and response.body.strip() == "ok":
        return {"status": "delivered", "message": "Mattermost accepted the message"}
    return failed(failure_of(response, key))

def failed(message):
    return {"status": "failed", "message": message}

def failure_of(response, key):
    """Says what Mattermost answered, without the webhook's key, and what to do."""
    code = response.http_status_code
    if code // 100 == 3:
        return "the server at the webhook's address answered %d, a redirect, and MMWatchdog follows no redirect. Copy the webhook's address from Mattermost again into the file that webhook_url_file names." % code

    # Mattermost's rate limit answers with a plain text, and says in the
    # header Retry-After how many seconds to wait.
    if code == 429:
        said = without_key(response.body.split("\n")[0], key) or "no text"
        return with_words("Mattermost answered 429: ", said, ". Mattermost takes no more requests from this node %s." % for_how_long(response.headers.get("Retry-After", "")))

    # Every other refusal of Mattermost is a JSON object with an id, such as
    # web.incoming_webhook.general.app_error, which is the same in every
    # language, a message in the language of the server, and the status
    # code. Recent versions of Mattermost answer every refusal of a webhook
    # with the same id and message, and the message names the webhook by its
    # key. So the module reports the id, or the message where the id is
    # empty, and only the status code tells the refusals apart.
    answer = json.decode(response.body, default = None)
    if type(answer) != "dict":
        answer = {}
    if type(answer.get("id")) == "string" and type(answer.get("status_code")) == "int":
        said = answer["id"]
        if said == "" and type(answer.get("message")) == "string":
            said = answer["message"]
        return with_words("Mattermost answered %d: " % code, without_key(said, key).removesuffix(".") or "no text", "." + advice_for(code))

    # Another server, such as a proxy or a gateway, answered.
    advice = ". Check the address in the file that webhook_url_file names, and what stands between this node and Mattermost."
    before = "the server at the webhook's address answered %d without the answer of Mattermost" % code
    if type(answer.get("message")) == "string" and answer["message"] != "":
        return with_words(before + ": ", without_key(answer["message"], key).removesuffix("."), advice)
    return before + advice

# Each piece of advice is short: MMWatchdog keeps the first 300 bytes of a module's
# message, and the advice comes last.
def advice_for(http_status_code):
    """Says what to do about a refusal of Mattermost."""
    if http_status_code in [400, 404]:
        return " Mattermost does not know this webhook: it was deleted, or its channel was archived. Make a new incoming webhook and write its address into the file that webhook_url_file names."
    if http_status_code == 403:
        return " Mattermost does not let this webhook post to its channel: the account that made it may have left the channel or its team. Make a new incoming webhook as a member of the channel."
    if http_status_code == 413:
        return " The message is larger than Mattermost, or a proxy in front of it, takes."
    if http_status_code == 501:
        return " An administrator of Mattermost has turned off incoming webhooks. Ask them to turn on Enable Incoming Webhooks in the System Console, under Integrations > Integration Management."
    return ""

def for_how_long(retry_after):
    """Turns the header Retry-After, a number of seconds, into words."""
    if retry_after == "1":
        return "for 1 second"
    if retry_after.isdigit():
        return "for %s seconds" % retry_after
    return "for a short time"

def without_key(text, key):
    """Returns a text of a server with the webhook's key replaced."""
    return text.replace(key, IN_PLACE_OF_THE_KEY)

def with_words(before, words, after):
    """Puts the words of a server between two parts of the module's message, cut so that the whole message fits in the bytes that MMWatchdog keeps.

    A cut falls between two characters, and "…" marks it.
    """
    most_bytes = MOST_BYTES_OF_A_RESULT_MESSAGE - len(before) - len(after)
    if len(words) > most_bytes:
        kept = ""
        for character in characters(words):
            if len(kept) + len(character) + len("…") > most_bytes:
                break
            kept += character
        words = kept + "…"
    return before + words + after

def characters(text):
    """Returns the characters of a text as a list, so that a text can be counted and cut by characters and not by bytes."""
    return list(text.codepoints())

def key_of_webhook(text):
    """Returns the key at the end of the address of a Mattermost incoming webhook, or "" for a text that is no such address.

    The address is one web address that starts with https:// or http://,
    without a space, a control character, a quote, or a backslash, and that
    ends with /hooks/ and the key, which is letters and digits.
    """
    after_the_scheme = ""
    for scheme in ["https://", "http://"]:
        if text.startswith(scheme):
            after_the_scheme = text[len(scheme):]
    server, hooks, key = after_the_scheme.rpartition("/hooks/")
    if server == "" or hooks == "" or key == "":
        return ""
    for character in characters(text):
        if ord(character) <= 32 or ord(character) == 127 or character in ['"', "\\"]:
            return ""
    for character in characters(key):
        if not (character.isalnum() and ord(character) < 128):
            return ""
    return key

def as_mattermost_line(text):
    """Returns the title of an alert as a line of Mattermost's Markdown that shows the title as it is.

    A backslash in front of a character makes Mattermost show the character
    and not format with it: `, *, ~, [, and ], and _ unless it stands
    between two letters or digits, where Mattermost formats nothing. A
    backslash itself gets one too, except before an ASCII letter or digit,
    as in mnt-data\\x2d1.mount: there it escapes nothing, and Mattermost's
    web app would show the two backslashes. An @ that a letter or a digit
    does not precede would mention somebody, so an invisible space follows
    it. Mattermost turns <!channel> into a mention of the channel, so its !
    gets a backslash too.
    """
    all_characters = characters(text)
    line = ""
    for index, character in enumerate(all_characters):
        before = all_characters[index - 1] if index > 0 else ""
        after = all_characters[index + 1] if index + 1 < len(all_characters) else ""
        if character == "\\" and after.isalnum() and ord(after) < 128:
            line += character
        elif character in ["\\", "`", "*", "~", "[", "]"]:
            line += "\\" + character
        elif character == "_" and not (before.isalnum() and after.isalnum()):
            line += "\\_"
        elif character == "!" and before == "<":
            line += "\\!"
        elif character == "@" and not before.isalnum():
            line += "@" + ZERO_WIDTH_SPACE
        else:
            line += character
    return line

def longest_row_of_backticks(text):
    """Returns the length of the longest row of backticks in a text."""
    longest = 0
    row = 0
    for character in characters(text):
        if character == "`":
            row += 1
            longest = max(longest, row)
        else:
            row = 0
    return longest

def cut(text, most_characters):
    """Cuts a text to a number of characters, and marks the cut with "…"."""
    all_characters = characters(text)
    if len(all_characters) <= most_characters:
        return text
    return "".join(all_characters[:most_characters - 1]) + "…"
