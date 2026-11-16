# discord: the official extra module of MMWatchdog that delivers alerts to Discord.
#
# MMWatchdog runs this module once for each alert of an alert destination that names
# it. The module posts the alert to a Discord webhook, through the method
# Execute Webhook of Discord's API: the alert's title with its severity in
# front, and below it the alert's body in a code block. README.md beside this
# file describes the inputs and the message, and module_description.yml lists
# the inputs for MMWatchdog.

# Discord takes at most 2,000 characters in the content of a message.
MOST_CHARACTERS_OF_A_MESSAGE = 2000

# MMWatchdog shows at most 300 bytes of the message with which a module answers.
MOST_BYTES_OF_A_RESULT = 300

THREE_BACKTICKS = "```"

# The message flag SUPPRESS_EMBEDS: Discord shows no preview of a web
# address in the message.
SUPPRESS_EMBEDS = 4

# Discord asks every program that uses its API to name itself.
USER_AGENT = "MMWatchdog discord module"

# The hosts of Discord's former name, and the host of today's name that
# takes their place. The former hosts refuse version 10 of the API.
FORMER_HOSTS = {
    "discordapp.com": "discord.com",
    "ptb.discordapp.com": "ptb.discord.com",
    "canary.discordapp.com": "canary.discord.com",
}

# Outside a code block, Discord reads these characters as formatting, or as
# the start or the end of a mention, a channel, an emoji, or a time. A
# backslash in front of one makes Discord show the character itself.
FORMATTING_CHARACTERS = ["\\", "*", "_", "~", "`", "|", "<", ">", "["]

# An invisible character. After the @ of @everyone and @here, it keeps the
# word from being a mention.
ZERO_WIDTH_SPACE = chr(0x200B)

def validate_inputs(inputs):
    if allowed_mentions_for(inputs) == None:
        return 'mention: set mention to one Discord mention: "<@USER_ID>" for a person, "<@&ROLE_ID>" for a role, "@everyone", or "@here"'
    return None

def verify_destination(inputs, file_contents):
    # The webhook object contains its token. Never include its body in a result.
    address = execute_address(file_contents["webhook_url_file"]).partition("?")[0]
    response = http_request(method = "GET", url = address, headers = {"User-Agent": USER_AGENT})
    if response.error:
        return (False, "MMWatchdog could not connect to Discord: " + response.error)
    if response.http_status_code // 100 == 5:
        return (False, "Discord verification answered HTTP %d" % response.http_status_code)
    answer = discord_said(response)
    if (response.http_status_code == 404 and answer.get("code") == 10015) or answer.get("code") == 50027:
        return (False, "Discord does not know this webhook or its token; replace the address in webhook_url_file")
    return (True, "Discord accepted the verification; no message was sent")

def deliver_alert(alert, inputs, file_contents):
    webhook_url = file_contents["webhook_url_file"]
    if not is_webhook_address(webhook_url):
        return failed("the file that webhook_url_file names must hold the address of one Discord webhook, which looks like https://discord.com/api/webhooks/123456789012345678/AbC-dEf_123, and nothing else")
    allowed_mentions = allowed_mentions_for(inputs)
    execute_url = execute_address(webhook_url)

    # A server at the address that is not Discord, such as a proxy or a
    # relay, can repeat the address or its path in its answer. Every text of
    # an answer that the module passes on has a placeholder in place of the
    # address and of the webhook's token.
    secrets = []
    for address in [execute_url, webhook_url]:
        secrets += [(address, "[address of the webhook]"), (address.partition("?")[0], "[address of the webhook]")]
    token = webhook_url.partition("/webhooks/")[2].partition("?")[0].partition("/")[2].partition("/")[0]
    if len(token) >= 8:
        secrets.append((token, "[token of the webhook]"))

    # The first line of the message is the alert's title with its severity
    # in front, such as "SOS: app-1: myapp/http failed". MMWatchdog writes that line.
    # With the input mention, the mention starts the line.
    first_line = as_discord_line(alert["title_with_alert_severity"])
    if "mention" in inputs:
        first_line = inputs["mention"] + " " + first_line

    # An alert for a destination that takes only the title has no body.
    body = alert.get("body", "")
    response = send_at_most(MOST_CHARACTERS_OF_A_MESSAGE, first_line, body, execute_url, allowed_mentions)

    # Discord counts characters as this module does, also emoji: it took
    # 2,000 characters with emoji in MMWatchdog's real service test. Should
    # Discord still answer that a text is too long, half the length always
    # fits, so the alert arrives instead of failing at every attempt.
    if not response.error and form_error_of_content(discord_said(response)).get("code") == "BASE_TYPE_MAX_LENGTH":
        response = send_at_most(MOST_CHARACTERS_OF_A_MESSAGE // 2, first_line, body, execute_url, allowed_mentions)
    if response.error:
        return failed("MMWatchdog could not send the message to Discord: " + response.error)

    # With wait=true, Discord answers a saved message with the message,
    # which has an id. Every other answer is a failure, and MMWatchdog tries again
    # later.
    answer = discord_said(response)
    if response.http_status_code // 100 == 2 and type(answer.get("id")) == "string":
        return {"status": "delivered", "message": "Discord accepted the message"}
    discord_message = answer.get("message")
    if type(discord_message) != "string":
        start = "the server at the webhook's address answered %d without Discord's JSON: " % response.http_status_code
        return failed_with_words(start, without_secrets(page_words(response.body), secrets), advice_for_another_answer(response.http_status_code))
    words = discord_message
    code = answer.get("code")
    if type(code) == "int":
        words += " (code %d)" % code
    content_error = form_error_of_content(answer).get("message")
    if type(content_error) == "string":
        words += ": content: " + content_error
    start = "Discord answered %d: " % response.http_status_code
    return failed_with_words(start, without_secrets(" ".join(words.split()), secrets), advice_for(response.http_status_code, code, answer))

def failed(message):
    return {"status": "failed", "message": message}

def failed_with_words(start, words, advice):
    """Answers failed with the start, the words of an answer as one sentence, and the advice. The words get only the bytes that the start and the advice leave, so that MMWatchdog shows the advice whole."""
    room = MOST_BYTES_OF_A_RESULT - len(start) - len(advice) - len(".")
    return failed(start + as_sentence(cut_to_bytes(words, room)) + advice)

def execute_address(webhook_url):
    """Returns the address to which the module posts the message.

    The address that Discord shows for a webhook names no version of the
    API, and so reaches version 6, which Discord marks as deprecated and
    whose answers say less. The module asks for version 10, which Discord's
    documentation describes, and on a host of Discord's former name it asks
    the host of today's name, because the former hosts refuse version 10.
    wait=true makes Discord answer only after it saved the message: without
    it, Discord answers 204 also for a message that it did not save.
    """
    scheme, _, rest = webhook_url.partition("://")
    host, slash, path = rest.partition("/")
    host = FORMER_HOSTS.get(host, host)
    if path.startswith("api/webhooks/"):
        path = "api/v10/" + path.removeprefix("api/")
    address = scheme + "://" + host + slash + path
    if "?" in address:
        return address + "&wait=true"
    return address + "?wait=true"

def send_at_most(most_characters, first_line, body, execute_url, allowed_mentions):
    """Sends the first line and, below it, the body in a code block, cut so that the message has at most the number of characters given."""
    content = first_line

    # Discord shows the text between two lines of three backticks as it is,
    # so *, _, and ~ in the output of a check stay those characters and do
    # not turn into bold or italic text. The first line and the two lines of
    # backticks, which take 9 characters with their line breaks, always
    # arrive.
    if body != "":
        most_characters_of_body = max(1, most_characters - len(characters(first_line)) - 9)
        content += "\n" + THREE_BACKTICKS + "\n" + cut(as_discord_code(body), most_characters_of_body) + "\n" + THREE_BACKTICKS

    # The address of the webhook is the secret. The primitive http_request
    # never writes an address into an error.
    return http_request(
        method = "POST",
        url = execute_url,
        headers = {"Content-Type": "application/json", "User-Agent": USER_AGENT},
        body = json.encode({"content": content, "allowed_mentions": allowed_mentions, "flags": SUPPRESS_EMBEDS}),
    )

def allowed_mentions_for(inputs):
    """Returns the allowed_mentions of the message: Discord notifies the person, the role, or the members that the input mention names, and nobody that a text of the alert names. Returns None for a mention that is not one of Discord's four forms."""
    if "mention" not in inputs:
        return {"parse": []}
    mention = inputs["mention"]
    if mention in ["@everyone", "@here"]:
        return {"parse": ["everyone"]}
    for start, kind in [("<@&", "roles"), ("<@", "users")]:
        number = mention.removeprefix(start).removesuffix(">")
        if mention.startswith(start) and mention.endswith(">") and is_number(number):
            return {"parse": [], kind: [number]}
    return None

def discord_said(response):
    """Returns Discord's answer, which is a JSON object, or an empty dictionary for another answer."""
    answer = json.decode(response.body, default = None)
    if type(answer) != "dict":
        return {}
    return answer

def form_error_of_content(answer):
    """Returns the first error that Discord found in the content of the message, such as {"code": "BASE_TYPE_MAX_LENGTH", "message": "Must be 2000 or fewer in length."}, or an empty dictionary."""
    errors = answer.get("errors")
    if type(errors) != "dict":
        return {}
    content = errors.get("content")
    if type(content) != "dict":
        return {}
    content_errors = content.get("_errors")
    if type(content_errors) != "list" or len(content_errors) == 0 or type(content_errors[0]) != "dict":
        return {}
    return content_errors[0]

def advice_for(http_status_code, code, answer):
    """Says what to do about an answer of Discord that is a failure."""
    if http_status_code in [401, 404]:
        return " The webhook was deleted, or the address in the file is wrong. Write the address of a webhook from Discord into the file that webhook_url_file names."
    if code == 220001:
        return " The webhook is in a forum channel, where a message needs a thread. Make a webhook for a text channel."
    if code == 40333:
        return " Cloudflare, in front of Discord, is blocking the request; Discord says that a proper User-Agent often resolves this. The module sends one, so check that no proxy on the way changes it."
    if http_status_code == 429:
        wait = answer.get("retry_after")
        if type(wait) not in ["int", "float"]:
            return " Discord takes no more messages from this webhook for a short time."
        if answer.get("global") == True:
            return " Discord takes no requests from this node's network address for %s. The limit counts the requests of every program there." % seconds(wait)
        return " Discord takes no more messages from this webhook for %s." % seconds(wait)
    if http_status_code // 100 == 5:
        return " Discord had a fault. MMWatchdog tries again."
    return ""

def advice_for_another_answer(http_status_code):
    """Says what to do about an answer that is not Discord's JSON."""
    if http_status_code in [403, 429]:
        return " Cloudflare, in front of Discord, answers so while it blocks this node's network address, for a time after many requests."
    if http_status_code // 100 == 5:
        return " Discord, or Cloudflare in front of it, had a fault. MMWatchdog tries again."
    return " Check the address in the file that webhook_url_file names, and any proxy between this node and Discord."

def page_words(text):
    """Returns the title of a web page, or else the first line of a text, on one line, or "no text"."""
    start = text.find("<title>")
    end = text.find("</title>")
    if start >= 0 and end > start:
        words = text[start + len("<title>"):end]
    else:
        words = text.strip().split("\n")[0]
    return " ".join(words.split()) or "no text"

def without_secrets(text, secrets):
    """Writes a placeholder in place of each secret in a text of an answer: the webhook's address in its forms, then its token."""
    for secret, placeholder in secrets:
        text = text.replace(secret, placeholder)
    return text

def as_sentence(words):
    """Ends the words of an answer with a period, unless they end with one."""
    if words.endswith("."):
        return words
    return words + "."

def seconds(wait):
    """Writes a time that Discord states in seconds, rounded up to whole seconds."""
    whole = int(wait)
    if whole < wait:
        whole += 1
    if whole == 1:
        return "1 second"
    return "%d seconds" % whole

def characters(text):
    """Returns the characters of a text as a list, so that a text can be counted and cut by characters and not by bytes."""
    return list(text.codepoints())

def cut(text, most_characters):
    """Cuts a text to a number of characters, and marks the cut with "…"."""
    all_characters = characters(text)
    if len(all_characters) <= most_characters:
        return text
    return "".join(all_characters[:most_characters - 1]) + "…"

def cut_to_bytes(text, most_bytes):
    """Cuts a text between two characters to a number of bytes, the mark "…" of the cut included."""
    if len(text) <= most_bytes:
        return text
    kept = []
    used = len("…")
    for character in characters(text):
        if used + len(character) > most_bytes:
            break
        kept.append(character)
        used += len(character)
    return "".join(kept) + "…"

def is_number(text):
    """Reports a text of one or more of the digits 0 to 9, as Discord writes the number of a person or a role."""
    if text == "":
        return False
    for character in characters(text):
        if character not in "0123456789":
            return False
    return True

def is_webhook_address(text):
    """Reports one web address that starts with https:// or http://, holds /webhooks/, and has no space, control character, quote, or backslash."""
    after_the_scheme = ""
    for scheme in ["https://", "http://"]:
        if text.startswith(scheme):
            after_the_scheme = text[len(scheme):]
    if after_the_scheme == "" or "/webhooks/" not in after_the_scheme:
        return False
    for character in characters(text):
        if ord(character) <= 32 or ord(character) == 127 or character in ['"', "\\"]:
            return False
    return True

def without_mentions(text):
    """Puts an invisible zero-width space after the @ of @everyone and @here, so that no text of an alert mentions anybody, whatever Discord makes of it."""
    return text.replace("@everyone", "@" + ZERO_WIDTH_SPACE + "everyone").replace("@here", "@" + ZERO_WIDTH_SPACE + "here")

def as_discord_line(text):
    """Returns a text of the alert as Discord shows it outside a code block: a backslash in front of each character that Discord reads as formatting or as part of a mention, so that Discord shows the text as it is."""
    escaped = []
    for character in characters(text):
        if character in FORMATTING_CHARACTERS:
            escaped.append("\\")
        escaped.append(character)
    return without_mentions("".join(escaped))

def as_discord_code(text):
    """Returns a text of the alert as Discord shows it inside a code block.

    Inside a code block Discord reads no formatting and no backslash. Only
    three backticks in a row end the code block, so the text gets them as
    three single quotes.
    """
    return without_mentions(text.replace(THREE_BACKTICKS, "'''"))
