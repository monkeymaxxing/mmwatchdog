# mongodb: the official extra module of MMWatchdog that checks a MongoDB server.
#
# MMWatchdog runs this module once for each run of a check that names it. The module
# asks the server through MongoDB's own shell, mongosh: whether the server
# answers, how many of its connections are in use, and, for a member of a
# replica set, whether the member is healthy and how far it is behind.
# README.md beside this file describes the inputs and the messages, and
# module_description.yml lists the inputs and the measurements for MMWatchdog.

# The shell writes its answer on a line that holds one of these two texts.
# The shell can write its own prompts in front of them on the same line.
ANSWER_MARK = "MMWATCHDOG_ANSWER "
ERROR_MARK = "MMWATCHDOG_ERROR "

# How long the shell looks for the server before it gives up, unless the
# connection string states its own time.
SERVER_SELECTION_TIMEOUT = "serverSelectionTimeoutMS=10000"

# What the shell runs. The address of the server takes the place of
# ADDRESS, as a JavaScript text. Everything is one statement, so that the
# shell, which reads its input line by line, runs all of it or nothing, and
# an error ends in one line that the module reads.
QUESTIONS = """
try {
  try { disableTelemetry(); } catch (error) {}
  const connection = new Mongo(ADDRESS);
  const admin = connection.getDB("admin");
  const hello = admin.runCommand({hello: 1});
  const server = admin.runCommand({serverStatus: 1});
  const answer = {
    replica_set: hello.setName || "",
    is_primary: hello.isWritablePrimary === true,
    is_secondary: hello.secondary === true,
    is_arbiter: hello.arbiterOnly === true,
    waits_for_a_replica_set: hello.isreplicaset === true,
    info: hello.info || "",
    connections_current: server.connections ? server.connections.current : null,
    connections_available: server.connections ? server.connections.available : null,
    state: "",
    lag_seconds: null,
    unhealthy_members: [],
    replica_status_error: "",
  };
  if (hello.setName) {
    try {
      const set = admin.runCommand({replSetGetStatus: 1});
      if (!set.ok) throw new Error(set.errmsg || "replSetGetStatus failed");
      const self = set.members.find(member => member.self);
      const primary = set.members.find(member => member.stateStr === "PRIMARY");
      answer.state = self ? self.stateStr : "";
      if (self && primary && self.optimeDate && primary.optimeDate) {
        answer.lag_seconds = Math.max(0, (primary.optimeDate - self.optimeDate) / 1000);
      }
      answer.unhealthy_members = set.members.filter(member => member.health !== 1).map(member => member.name);
    } catch (error) {
      answer.replica_status_error = String(error && error.name ? error.name + ": " + error.message : error).split("\\n")[0];
    }
  }
  print("MMWATCHDOG_ANSWER " + JSON.stringify(answer));
} catch (error) {
  print("MMWATCHDOG_ERROR " + String(error && error.name ? error.name + ": " + error.message : error).split("\\n")[0]);
}
"""

def run_check(inputs, file_contents):
    address = "mongodb://127.0.0.1:27017/"
    password = ""
    if "connection_string_file" in inputs:
        address = file_contents["connection_string_file"].strip()
        scheme, separator, rest = address.partition("://")
        if scheme not in ["mongodb", "mongodb+srv"] or not separator or not rest or "\n" in address or " " in address:
            return unknown("the file that connection_string_file names must hold one MongoDB connection string that starts with mongodb:// or mongodb+srv://, and nothing else")

        # The account and its password are in front of the last @. A
        # character there that separates the parts of a connection string
        # would make the shell take a piece of the password for something
        # else, and then show that piece in an error.
        account, at, _ = rest.rpartition("@")
        if at:
            name, colon, password = account.partition(":")
            for character in ["/", "@", "?", "#", "[", "]"]:
                if character in account or ":" in password:
                    return unknown("the connection string in the file that connection_string_file names has a user name or a password with a character that must be written in its percent form there, such as %40 for @, %3A for a colon, and %2F for a slash")

    # The shell gives up after 30 seconds by itself. A shorter time lets the
    # check say that the server did not answer, within its own time limit.
    asked = address
    if "serverselectiontimeoutms" not in address.lower():
        if "?" in address:
            asked += "&" + SERVER_SELECTION_TIMEOUT
        elif "/" in address.partition("://")[2]:
            asked += "?" + SERVER_SELECTION_TIMEOUT
        else:
            asked += "/?" + SERVER_SELECTION_TIMEOUT

    # The address can hold a password, so it reaches the shell on its
    # standard input, inside the questions, and never as an argument: the
    # arguments of a program show in the process list.
    ran = run_command(["mongosh", "--nodb", "--quiet", "--norc"], standard_input = QUESTIONS.replace("ADDRESS", json.encode(asked)))
    if ran.error:
        return unknown("MMWatchdog cannot run mongosh, the shell of MongoDB: %s. The check needs that program on this node" % ran.error)
    answer = None
    reason = ""
    for line in ran.standard_output.split("\n"):
        if ANSWER_MARK in line:
            answer = json.decode(line[line.index(ANSWER_MARK) + len(ANSWER_MARK):], None)
        elif ERROR_MARK in line:
            reason = line[line.index(ERROR_MARK) + len(ERROR_MARK):].strip()
    if type(answer) != "dict":
        said = without_secrets(reason or what_the_shell_says(ran.standard_error + "\n" + ran.standard_output), [asked, address, password])
        return failed("the MongoDB server did not answer the check: " + (said or "mongosh ended with exit code %d" % ran.exit_code))

    if answer.get("replica_status_error"):
        return unknown("MongoDB could not report its replica set status: " + without_secrets(answer["replica_status_error"], [asked, address, password]))

    measurements = {}
    current, available = answer.get("connections_current"), answer.get("connections_available")
    if type(current) in ["int", "float"] and type(available) in ["int", "float"] and current + available > 0:
        measurements["connections_used_percent"] = 100.0 * current / (current + available)

    # A primary is 0 seconds behind itself. Which member is the primary
    # changes by itself, so one check with a limit fits every member.
    if type(answer.get("lag_seconds")) in ["int", "float"]:
        measurements["replication_lag_seconds"] = answer["lag_seconds"]

    if not answer["replica_set"]:
        if answer["is_primary"]:
            return {"status": "ok", "message": "the MongoDB server answers; it is a server of its own, in no replica set", "measurements": measurements}

        # A server that was started for a replica set and belongs to none:
        # nobody set the replica set up, or the server was removed from it.
        message = "the MongoDB server answers and takes no changes: it was started for a replica set and is a member of none"
        if answer.get("info"):
            message += " (%s)" % answer["info"]
        return {"status": "failed", "message": message, "measurements": measurements}
    described = "the MongoDB server answers; it is a member of the replica set %s in the state %s" % (answer["replica_set"], answer["state"] or "that it does not name")

    # A member that is neither the primary, nor a secondary, nor an arbiter
    # does not serve the replica set: it starts, recovers, or was removed.
    if not answer["is_primary"] and not answer["is_secondary"] and not answer.get("is_arbiter"):
        return {"status": "failed", "message": described + ", which is neither PRIMARY, SECONDARY, nor ARBITER", "measurements": measurements}
    if answer["unhealthy_members"]:
        return {"status": "failed", "message": "%s; this member cannot reach %s" % (described, ", ".join(answer["unhealthy_members"])), "measurements": measurements}
    return {"status": "ok", "message": described, "measurements": measurements}

def what_the_shell_says(written):
    """Returns the line that the shell wrote about its failure: the first line that names an error, or the first line."""
    lines = [line.strip() for line in written.split("\n") if line.strip()]
    for line in lines:
        if "Error" in line:
            return line
    return lines[0] if lines else ""

def without_secrets(text, secrets):
    """Returns the text without the address of the server and without its password."""
    for secret in secrets:
        if secret:
            text = text.replace(secret, "the server's address" if "://" in secret else "(the password)")
    return text

def failed(message):
    return {"status": "failed", "message": message}

def unknown(message):
    return {"status": "unknown", "message": message}
