package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"strings"

	"mmwatchdog/internal/primitives"
)

// MySQL is the inputs of the mysql check module, which asks a MySQL or a
// MariaDB server a question through its client program and measures how
// far a replica is behind, how many of the server's connections are in
// use, and how long InnoDB's history list is. A check without inputs asks
// the server on this node through its local socket, as root.
type MySQL struct {
	DatabaseServer `yaml:",inline"`
	LoginUser      string `yaml:"login_user" json:"login_user,omitempty"`
}

func (m *MySQL) Validate(v InputValidation) *FieldError {
	if problem := m.DatabaseServer.validate(v, "login_user", m.LoginUser); problem != nil {
		return problem
	}
	// A MySQL or a MariaDB server has a port only on the network. Through
	// the local socket MySQL's client would ask the server of the standard
	// socket and say nothing about the port.
	if m.DatabaseServerPort != 0 && m.DatabaseServerHost == "" {
		return &FieldError{"database_server_port", "needs database_server_host: without it the check uses the server's local socket, which has no port. For a server on this node at a port, state database_server_host: 127.0.0.1"}
	}
	return nil
}

// mysqlClientPrograms are the names of the client program: MariaDB's, and
// the one of MySQL, which older MariaDB releases have too.
var mysqlClientPrograms = []string{"mariadb", "mysql"}

// connectionSecurityArguments are the arguments that tell the client program
// how the connection is protected. The two makers' clients take different
// ones, and each refuses those of the other. authorities is the file of
// certificate authorities for verified_tls.
//
// Through the local socket, MySQL's client and the older ones of MariaDB
// encrypt nothing. MariaDB's client from 11.4 encrypts there too and
// verifies the server's certificate, and so fails for a server whose
// certificate this node does not know. A local socket needs no encryption,
// so that client gets --skip-ssl.
//
// MariaDB's client verifies against its --ssl-ca alone only when the check
// states one: without it, a newer one also accepts a certificate that the
// server proves with the password of the login. So the check always states
// one. MySQL's client logs in to MySQL without encryption only when it may
// ask the server for its public key.
func (m *MySQL) connectionSecurityArguments(mariaDB bool, authorities string) []string {
	switch {
	case m.DatabaseServerHost == "" && mariaDB:
		return []string{"--skip-ssl"}
	case m.DatabaseServerHost == "":
		return nil // MySQL's client takes --skip-ssl for an unknown argument.
	case m.ConnectionSecurity == databaseVerifiedTLS && mariaDB && m.DangerouslySkipServerCertificateCheck:
		return []string{"--ssl", "--skip-ssl-verify-server-cert"}
	case m.ConnectionSecurity == databaseVerifiedTLS && mariaDB:
		return []string{"--ssl", "--ssl-verify-server-cert", "--ssl-ca=" + authorities}
	case m.ConnectionSecurity == databaseVerifiedTLS && m.DangerouslySkipServerCertificateCheck:
		return []string{"--ssl-mode=REQUIRED"}
	case m.ConnectionSecurity == databaseVerifiedTLS:
		return []string{"--ssl-mode=VERIFY_IDENTITY", "--ssl-ca=" + authorities}
	case mariaDB:
		return []string{"--skip-ssl"}
	}
	return []string{"--ssl-mode=DISABLED", "--get-server-public-key"}
}

// MySQLQuestion is the question of one run, one statement on each line, up
// to its last statement, which asks for the status of a replica. The client
// program reads the question on its standard input, where --force makes it
// go on after a statement that the server refuses. It says which statement
// that was by its line: "ERROR 1227 (42000) at line 3: ...". With
// --execute it would ask nothing after the refused statement.
//
// The history list length of InnoDB has two sources. MariaDB gives it to
// every account as the status variable Innodb_history_list_length. MySQL
// has only InnoDB's counter trx_rseg_history_len, which an account reads
// with the privilege PROCESS. The third statement asks for every column of
// the counter, because MySQL says whether the counter is on in the column
// STATUS and MariaDB in the column ENABLED.
const MySQLQuestion = "SELECT @@max_connections AS max_connections;\n" +
	"SHOW GLOBAL STATUS WHERE Variable_name IN ('Threads_connected', 'Innodb_history_list_length');\n" +
	"SELECT * FROM information_schema.INNODB_METRICS WHERE NAME = 'trx_rseg_history_len';\n"

// mysqlInnoDBCounterLine and mysqlReplicaStatusLine are the lines of the
// question that ask for InnoDB's counter and for the status of a replica.
const mysqlInnoDBCounterLine, mysqlReplicaStatusLine = 3, 4

// mysqlRefusal is a line in which the client program says that the server
// refused the statement of one line of the question.
var mysqlRefusal = regexp.MustCompile(`(?m)^ERROR \d+ \([^)]*\) at line (\d+): .*$`)

// mysqlRefusals returns what the client program says about each statement
// that the server refused, by the line of the question that holds it.
// MariaDB's client writes the refused statement in front of each such
// line, which this leaves out.
func mysqlRefusals(said string) map[int]string {
	refusals := map[int]string{}
	for _, refusal := range mysqlRefusal.FindAllStringSubmatch(said, -1) {
		line, _ := strconv.Atoi(refusal[1])
		refusals[line] = strings.TrimSpace(refusal[0])
	}
	return refusals
}

// mysqlReplicaStatusStatements are the statements that ask for the status
// of a replica, in the order in which a check tries them: a server knows
// some of them and answers a syntax error to the others. MariaDB shows
// every source that a replica follows only to a statement with ALL: without
// that word it shows the one source that has no name. MySQL shows every
// source to its one statement. Newer releases say REPLICA, older ones
// SLAVE.
var mysqlReplicaStatusStatements = []string{"SHOW ALL REPLICAS STATUS", "SHOW REPLICA STATUS", "SHOW ALL SLAVES STATUS", "SHOW SLAVE STATUS"}

func (m *MySQL) Run(ctx context.Context, r *CheckRun) Observation {
	password, problem := m.password()
	if problem != nil {
		return *problem
	}
	program := mysqlClientPrograms[len(mysqlClientPrograms)-1]
	for _, name := range mysqlClientPrograms {
		if primitives.IsExecutableFile(primitives.NodeFile("/usr/bin/"+name)) || primitives.IsExecutableFile(primitives.NodeFile("/usr/local/bin/"+name)) {
			program = name
			break
		}
	}
	// Through the local socket, the server takes root of this node for its
	// own administrator. A check that states a login needs no rights on
	// this node, so its client runs as the unprivileged account.
	// --no-defaults does not suppress MySQL's .mylogin.cnf. MariaDB
	// ignores this variable, so both clients can use the same environment.
	account, environment := primitives.MySQLAdministratorAccount, map[string]string{"MYSQL_TEST_LOGIN_FILE": "/dev/null"}
	arguments := []string{program, "--no-defaults", "--batch", "--vertical", "--force", "--connect-timeout=10"}
	if m.LoginUser != "" {
		account = primitives.UnprivilegedAccount
	}
	// The client under the name mariadb is MariaDB's. The one under the
	// name mysql is MySQL's or one of MariaDB, and says which.
	mariaDB, authorities := program == "mariadb", ""
	if !mariaDB {
		version, err := primitives.RunCommand(ctx, primitives.Command{Arguments: []string{program, "--no-defaults", "--version"}, RunAs: &primitives.ExecOptions{RunAsUserAccount: account, RunFromDirectory: "/", Env: environment}})
		var notFound *exec.Error
		switch {
		case errors.As(err, &notFound):
			return Result("unknown", fmt.Sprintf("MMWatchdog cannot run %s, the client program of MySQL: %v. The check needs that program on this node", program, notFound.Err))
		case err != nil || version.ExitCode != 0:
			return Result("unknown", primitives.DetailLine("MMWatchdog cannot learn whether the client program "+program+" is MySQL's or MariaDB's, and the two take different arguments: "+program+" --version gave no answer"))
		}
		mariaDB = strings.Contains(version.StandardOutput, "MariaDB")
	}
	serverHost, serverPort := m.DatabaseServerHost, m.DatabaseServerPort
	if mariaDB && m.ConnectionSecurity == databaseVerifiedTLS && m.DangerouslySkipServerCertificateCheck {
		address, stop, err := primitives.MySQLTLSLoginGuard(ctx, net.JoinHostPort(serverHost, strconv.Itoa(serverPort)))
		if err != nil {
			return Result("unknown", "MMWatchdog cannot protect the MariaDB login: "+err.Error())
		}
		defer stop()
		var port string
		serverHost, port, _ = net.SplitHostPort(address)
		serverPort, _ = strconv.Atoi(port)
	}
	// A check states to the client how it reaches the server, so that a
	// file of the client's own settings cannot send it to another one.
	if m.DatabaseServerHost == "" {
		arguments = append(arguments, "--protocol=SOCKET")
	} else {
		arguments = append(arguments, "--host="+serverHost, "--protocol=TCP")
		if m.ConnectionSecurity == databaseVerifiedTLS {
			var problem *Observation
			if authorities, problem = m.trustedCertificateAuthorities(ctx); problem != nil {
				return *problem
			}
		}
	}
	if m.DatabaseServerPort != 0 {
		arguments = append(arguments, "--port="+strconv.Itoa(serverPort))
	}
	if m.LoginUser != "" {
		arguments = append(arguments, "--user="+m.LoginUser)
	}
	arguments = append(arguments, m.connectionSecurityArguments(mariaDB, authorities)...)
	if password != "" {
		environment["MYSQL_PWD"] = password
	}
	question := MySQLQuestion
	ask := func(question string) ([]map[string]string, map[int]string, *Observation) {
		ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: arguments, StandardInput: []byte(question), RunAs: &primitives.ExecOptions{RunAsUserAccount: account, RunFromDirectory: "/", Env: environment}})
		// With --force the client goes on after a statement that the server
		// refuses, so the answers to the other statements arrive also when
		// one of them fails.
		if err == nil && strings.Contains(ran.StandardOutput, "max_connections:") {
			return verticalRows(ran.StandardOutput), mysqlRefusals(ran.StandardError), nil
		}
		_, failed := askDatabaseClient(ctx, "MySQL", account, append(arguments, "--execute=SELECT 1"), environment)
		if failed == nil {
			o := Result("unknown", primitives.DetailLine("the client program "+program+" gave an answer that MMWatchdog does not understand: "+primitives.Clip(ran.StandardOutput+ran.StandardError, 200)))
			failed = &o
		}
		return nil, nil, failed
	}
	// The check starts with the statement that the server understood at
	// the run before, which it keeps in its memory, and goes on to the next
	// one while the server answers a syntax error (1064).
	statement, _ := r.Memory.Value.(int)
	var rows []map[string]string
	var refused map[int]string
	var failed *Observation
	for ; statement < len(mysqlReplicaStatusStatements); statement++ {
		rows, refused, failed = ask(question + mysqlReplicaStatusStatements[statement])
		if failed != nil || !strings.HasPrefix(refused[mysqlReplicaStatusLine], "ERROR 1064 ") {
			r.Memory.Value = statement
			break
		}
	}
	if failed != nil {
		return *failed
	}
	// The first statement answers with one row, the status variables with
	// one row each, and InnoDB's counter with one row. The status of a
	// replica has one row for each source that the server follows, which
	// MySQL calls a channel and MariaDB a connection. The channels of a
	// group replication are no sources: such a member has no thread that
	// receives, by its design.
	answer, status, counter := map[string]string{}, map[string]string{}, map[string]string(nil)
	channels, inGroupReplication := []map[string]string{}, false
	for _, row := range rows {
		_, new := row["Replica_IO_Running"]
		_, old := row["Slave_IO_Running"]
		switch {
		case row["Variable_name"] != "":
			status[row["Variable_name"]] = row["Value"]
		case row["NAME"] == "trx_rseg_history_len":
			counter = row
		case !new && !old:
			maps.Copy(answer, row)
		case strings.HasPrefix(row["Channel_Name"], "group_replication_"):
			inGroupReplication = true
		default:
			channels = append(channels, row)
		}
	}
	o := Result("ok", "the MySQL server answers; it is no replica")
	o.Measurements = map[string]Measurement{}
	unavailable := map[string]string{}
	if connections, err := strconv.ParseFloat(status["Threads_connected"], 64); err == nil {
		if allowed, err := strconv.ParseFloat(answer["max_connections"], 64); err == nil && allowed > 0 {
			o.Measurements["connections_used_percent"] = Measurement{100 * connections / allowed, "percent"}
		}
	}
	if length, reason := innoDBHistoryListLength(status["Innodb_history_list_length"], counter, refused[mysqlInnoDBCounterLine]); reason == "" {
		o.Measurements["innodb_history_list_length"] = Measurement{length, "transactions"}
	} else {
		unavailable["innodb_history_list_length"] = reason
	}
	// The status of a replica: for each source its two threads, and how
	// far the replica is behind. The columns have the words of the
	// server's release.
	first := func(row map[string]string, names ...string) string {
		for _, name := range names {
			if value, has := row[name]; has {
				return value
			}
		}
		return ""
	}
	if len(channels) == 0 {
		switch said := refused[mysqlReplicaStatusLine]; {
		case strings.HasPrefix(said, "ERROR 1227 "):
			unavailable["replication_lag_seconds"] = "the account of the check may not ask for the status of a replica: it needs the privilege REPLICATION CLIENT, or REPLICA MONITOR on MariaDB"
			o.Message = "the MySQL server answers"
		case said != "":
			// Without an answer MMWatchdog does not know whether the server is a
			// replica, so it gives no number.
			unavailable["replication_lag_seconds"] = primitives.DetailLine("the MySQL server did not say whether it is a replica: " + said)
			o.Message = "the MySQL server answers"
		case inGroupReplication:
			unavailable["replication_lag_seconds"] = "the MySQL server is a member of a group replication, and this check has no number for how far such a member is behind"
			o.Message = "the MySQL server answers; it is a member of a group replication"
		default:
			// A server that follows no source is behind no server, so it
			// is 0 seconds behind. One check with a limit then fits a
			// source and its replicas alike, and stays right when a
			// replica becomes the source.
			o.Measurements["replication_lag_seconds"] = Measurement{0, "seconds"}
		}
		return withLimits(r.Check, o, unavailable)
	}
	// A replica that does not receive or does not apply the changes of one
	// of its sources falls behind without end, and the server gives no
	// number for that.
	behind, numbered := 0.0, true
	for _, channel := range channels {
		receiving, applying := first(channel, "Replica_IO_Running", "Slave_IO_Running"), first(channel, "Replica_SQL_Running", "Slave_SQL_Running")
		if receiving != "Yes" || applying != "Yes" {
			source := "its source"
			if name := channel["Channel_Name"]; name != "" {
				source = "its source of the channel " + name
			} else if name := channel["Connection_name"]; name != "" {
				source = "its source of the connection " + name
			}
			o.Status = "failed"
			o.Message = primitives.DetailLine("the MySQL replica does not follow " + source + ": " + replicaThread("receiving", receiving) + "; " + replicaThread("applying", applying))
			for _, name := range []string{"Last_IO_Error", "Last_SQL_Error"} {
				if said := channel[name]; said != "" {
					o.Details = append(o.Details, primitives.DetailLine(name+": "+said))
				}
			}
			return o
		}
		seconds, err := strconv.ParseFloat(first(channel, "Seconds_Behind_Source", "Seconds_Behind_Master"), 64)
		numbered = numbered && err == nil
		behind = max(behind, seconds)
	}
	sources := "its source"
	if len(channels) > 1 {
		sources = fmt.Sprintf("the %d sources that it follows", len(channels))
	}
	if numbered {
		o.Measurements["replication_lag_seconds"] = Measurement{behind, "seconds"}
		o.Message = "the MySQL server answers; it is a replica, " + primitives.FormatReading(behind) + " seconds behind " + sources
	} else {
		unavailable["replication_lag_seconds"] = "the MySQL replica gives no number for how far it is behind"
		o.Message = "the MySQL server answers; it is a replica"
	}
	return withLimits(r.Check, o, unavailable)
}

// innoDBHistoryListLength returns the history list length of InnoDB, or
// says why the server gave none. variable is MariaDB's status variable,
// which comes first, and counter the row of InnoDB's counter
// trx_rseg_history_len. refused is what the client program said about the
// statement that asks for the counter, if the server refused it.
func innoDBHistoryListLength(variable string, counter map[string]string, refused string) (float64, string) {
	if length, err := strconv.ParseFloat(variable, 64); err == nil {
		return length, ""
	}
	switch {
	case strings.HasPrefix(refused, "ERROR 1227 "):
		return 0, "the account of the check may not read the InnoDB history list length: it needs the privilege PROCESS"
	case refused != "":
		return 0, primitives.DetailLine("the MySQL server did not give the InnoDB history list length: " + refused)
	case counter == nil:
		return 0, "the MySQL server gave no InnoDB history list length"
	case counter["STATUS"] == "disabled":
		// A counter that is off keeps the number that it had then.
		return 0, "InnoDB's counter trx_rseg_history_len is turned off on the MySQL server, so its number is old: turn it on with SET GLOBAL innodb_monitor_enable = 'trx_rseg_history_len'"
	}
	length, err := strconv.ParseFloat(counter["COUNT"], 64)
	if err != nil {
		return 0, primitives.DetailLine("the MySQL server gave an InnoDB history list length that MMWatchdog does not understand: " + counter["COUNT"])
	}
	return length, ""
}

// replicaThread says, in the words of a message, whether one of the two
// threads of a replica runs. running is the server's own word for it: Yes,
// No, or Connecting.
func replicaThread(doing, running string) string {
	if running == "Yes" {
		return "it is " + doing + " changes"
	}
	return "it is not " + doing + " changes (" + running + ")"
}

// verticalRows reads what the client writes with --vertical: a line of
// stars in front of each row, and then one line for each column of the row,
// with the name of the column, a colon, and the value.
func verticalRows(written string) []map[string]string {
	rows := []map[string]string{}
	for _, line := range strings.Split(written, "\n") {
		if strings.HasPrefix(line, "*") {
			rows = append(rows, map[string]string{})
			continue
		}
		if name, value, found := strings.Cut(line, ":"); found && len(rows) > 0 {
			rows[len(rows)-1][strings.TrimSpace(name)] = strings.TrimSpace(value)
		}
	}
	return rows
}

// mysqlDescription is what "mmwatchdog describe-module mysql" shows, and what
// validation knows about the module's inputs.
var mysqlDescription = ModuleDescription{
	Summary: "Asks a MySQL or a MariaDB server a question through its client program, and measures how far a replica is behind, how many connections are in use, and how long InnoDB's history list is. Without inputs, the check asks the server on this node.",
	UsedAs:  UsedAsCheck,
	Inputs:  databaseServerInputs("MySQL or MariaDB", "login_user", "as root, which the server takes for its own administrator without a password"),
	Measurements: []ModuleMeasurement{
		{Name: "replication_lag_seconds", Unit: "seconds", Description: "How far a replica is behind its source. A server that is no replica is 0 seconds behind, so one check with a limit fits a source and its replicas."},
		{Name: "connections_used_percent", Unit: "percent", Description: "The open connections as a share of the connections that the server allows."},
		{Name: "innodb_history_list_length", Unit: "transactions", Description: "InnoDB's history list length: the committed transactions whose old row versions InnoDB keeps and has not purged yet. It grows while a transaction stays open for long, and the server gets slower. A login reads it on MySQL with the privilege PROCESS, and on MariaDB without one."},
	},
	Examples: []ModuleExample{
		{
			Description:   "The MariaDB or MySQL server of this node answers.",
			Configuration: "mysql: {}\nalert_severity: sos\n",
		},
		{
			Description:   "The database server on another machine answers a login over an encrypted connection, its certificate is valid for its name, and fewer than 80 percent of its connections are in use.",
			Configuration: "mysql:\n  database_server_port: 3306\n  database_server_host: db-1.internal.example.com\n  connection_security: verified_tls\n  login_user: mmwatchdog_monitoring\n  login_password_file: /etc/mmwatchdog/secrets/mysql-mmwatchdog-monitoring\nfail_when_measurement_above:\n  connections_used_percent: 80\nalert_severity: sos\n",
		},
	},
}
