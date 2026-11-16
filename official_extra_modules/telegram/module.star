# telegram: the official extra module of MMWatchdog that delivers alerts to Telegram.
#
# MMWatchdog runs this module once for each alert of an alert destination that names
# it. The module sends the alert to one Telegram chat, as a bot, through the
# method sendMessage of the Telegram Bot API: the alert's title with its
# severity in front, and below it the alert's body. README.md beside this
# file describes the inputs and the message, and module_description.yml
# lists the inputs for MMWatchdog.

# Telegram takes at most 4,096 characters in a message.
MOST_CHARACTERS_OF_A_MESSAGE = 4096

def validate_inputs(inputs):
    if "bot_api_server_url" in inputs and not is_web_address(inputs["bot_api_server_url"]):
        return "bot_api_server_url: use one web address starting with https:// or http://, such as https://api.telegram.org; this is the service's API address"
    return None

def verify_destination(inputs, file_contents):
    response = http_request(
        method = "POST",
        url = inputs.get("bot_api_server_url", "https://api.telegram.org").removesuffix("/") + "/bot" + file_contents["bot_token_file"] + "/getChat",
        headers = {"Content-Type": "application/json"},
        body = json.encode({"chat_id": inputs["chat_id"]}),
    )
    if response.error:
        return (False, "MMWatchdog could not connect to Telegram: " + response.error)
    if response.http_status_code // 100 == 5:
        return (False, "Telegram verification answered HTTP %d" % response.http_status_code)
    answer = telegram_said(response)
    if answer.get("ok") == False:
        if response.http_status_code == 401 and answer.get("error_code") == 401 and description_of(answer) == "Unauthorized":
            return (False, "Telegram does not know this bot token; replace the content of bot_token_file")
        if response.http_status_code == 400 and answer.get("error_code") == 400 and description_of(answer) == "Bad Request: chat not found":
            return (False, "Telegram does not know this chat; correct chat_id and add the bot to the chat")
    return (True, "Telegram accepted the verification; no message was sent")

def deliver_alert(alert, inputs, file_contents):
    bot_token = file_contents["bot_token_file"]
    if not is_bot_token(bot_token):
        return failed("the file that bot_token_file names must hold one bot token, which looks like 123456789:AAE-x1y2z3, and nothing else")
    bot_api_server_url = inputs.get("bot_api_server_url", "https://api.telegram.org")

    # The token is part of the address of every request to the Bot API. The
    # primitive http_request never writes an address into an error.
    send_message_url = bot_api_server_url.removesuffix("/") + "/bot" + bot_token + "/sendMessage"

    # The message is the alert's title with its severity in front, such as
    # "SOS: app-1: myapp/http failed", and, below it, the alert's body. MMWatchdog
    # writes both. An alert for a destination that takes only the title has
    # no body. The text goes out as it is: the request names no parse mode,
    # so Telegram formats nothing.
    text = alert["title_with_alert_severity"]
    if "body" in alert:
        text += "\n" + alert["body"]

    response = send_at_most(MOST_CHARACTERS_OF_A_MESSAGE, text, inputs["chat_id"], send_message_url)

    # Telegram counts characters as this module does, also emoji: it took
    # 4,096 characters with emoji in MMWatchdog's real service test. Should
    # Telegram still answer that a text is too long, half the length always
    # fits, so the alert arrives instead of failing at every attempt.
    if not response.error and "message is too long" in description_of(telegram_said(response)):
        response = send_at_most(MOST_CHARACTERS_OF_A_MESSAGE // 2, text, inputs["chat_id"], send_message_url)
    if response.error:
        return failed("MMWatchdog could not send the message to Telegram: " + response.error)

    # Telegram answers with "ok": true when it took the message. Every other
    # answer is a failure, and MMWatchdog tries again later.
    answer = telegram_said(response)
    if answer.get("ok") == True:
        return {"status": "delivered", "message": "Telegram accepted the message"}
    description = description_of(answer)
    if description == "":
        return failed("the Bot API server answered %d without the JSON that Telegram answers with. Check bot_api_server_url, and what stands between this node and that server." % response.http_status_code)
    return failed("Telegram answered %d: %s.%s" % (response.http_status_code, "".join(characters(description)[:200]), advice_for(response.http_status_code, description, answer.get("parameters"))))

def failed(message):
    return {"status": "failed", "message": message}

def description_of(answer):
    """Returns the text with which Telegram says what is wrong, or "" for an answer that has no such text."""
    description = answer.get("description", "")
    if type(description) != "string":
        return ""
    return description

def send_at_most(most_characters, text, chat_id, send_message_url):
    """Sends the first characters of the text, up to the number given, and marks a cut with "…"."""
    all_characters = characters(text)
    if len(all_characters) > most_characters:
        text = "".join(all_characters[:most_characters - 1]) + "…"
    return http_request(
        method = "POST",
        url = send_message_url,
        headers = {"Content-Type": "application/json"},
        # Telegram would add a preview of the first web address in the text.
        body = json.encode({"chat_id": chat_id, "text": text, "link_preview_options": {"is_disabled": True}}),
    )

def telegram_said(response):
    """Returns Telegram's answer, which is a JSON object, or an empty dictionary for another answer."""
    answer = json.decode(response.body, default = None)
    if type(answer) != "dict":
        return {}
    return answer

def advice_for(http_status_code, description, parameters):
    """Says what to do about an answer of Telegram that is a failure."""
    if type(parameters) != "dict":
        parameters = {}
    new_chat_id = parameters.get("migrate_to_chat_id")
    if type(new_chat_id) == "int":
        return ' The group became a supergroup, which has a new number. Set chat_id to "%d".' % new_chat_id
    if "chat not found" in description:
        return " Check chat_id, and that the bot is a member of the chat."
    wait_seconds = parameters.get("retry_after")
    if type(wait_seconds) == "int":
        return " Telegram takes no more messages from this bot for %d seconds." % wait_seconds
    if http_status_code in [401, 404]:
        return " Telegram does not know this bot token. Get the token of the bot from @BotFather in Telegram and write it into the file that bot_token_file names."
    if http_status_code == 403:
        return " The bot may not write to this chat. Add the bot to the group or the channel. A person must first send the bot a message."
    return ""

def characters(text):
    """Returns the characters of a text as a list, so that a text can be counted and cut by characters and not by bytes."""
    return list(text.codepoints())

def is_bot_token(text):
    """Reports a text of the form of a bot token: digits, a colon, and letters, digits, underscores, and hyphens."""
    number, colon, secret = text.partition(":")
    if colon != ":" or number == "" or secret == "" or not number.isdigit():
        return False
    for character in characters(secret):
        if not (character.isalnum() and ord(character) < 128) and character not in ["_", "-"]:
            return False
    for character in characters(number):
        if ord(character) >= 128:
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
