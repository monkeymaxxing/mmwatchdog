package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// PostgreSQL is the inputs of the postgresql check module, which asks a
// PostgreSQL server a question through its client program, psql, and
// measures how far a replica is behind, how many of the server's
// connections are in use, how many of its block reads its shared buffers
// served, how long its write-ahead log waits to be archived, and how far
// its transaction IDs are toward wraparound. A check without inputs asks
// the server on this node through its local socket, as the operating
// system account postgres.
type PostgreSQL struct {
	DatabaseServer `yaml:",inline"`
	LoginRole      string `yaml:"login_role" json:"login_role,omitempty"`
}

func (p *PostgreSQL) Validate(v InputValidation) *FieldError {
	return p.DatabaseServer.validate(v, "login_role", p.LoginRole)
}

// TimeOfEachRun is the sample of the first run: the cache hit ratio needs
// two readings of the server's counters of block reads.
func (p *PostgreSQL) TimeOfEachRun() (time.Duration, string) {
	return CPUSampleWindow, "the initial sample of the PostgreSQL server's block reads"
}

// postgresqlBlockReads asks for the server's counters of the blocks that
// its shared buffers served and of those that it read from outside them,
// over all its databases: the buffers are one for the whole server.
const postgresqlBlockReads = "(SELECT sum(blks_hit) FROM pg_stat_database), (SELECT sum(blks_read) FROM pg_stat_database)"

// postgresqlArchiveWait asks how long the oldest finished segment of the
// write-ahead log has waited to be archived, 0 when none waits. PostgreSQL
// lists the segments that wait only to its administrators and to members
// of pg_monitor, and checks that right before it runs the statement, so
// only the question of a check without a login asks it.
const postgresqlArchiveWait = "COALESCE((SELECT EXTRACT(EPOCH FROM now() - min(modification)) FROM pg_ls_archive_statusdir() WHERE name LIKE '%.ready'), 0)"

// postgresqlQuestion asks, in one statement, whether the server is a
// replica, how old the last change is that a replica has applied, whether a
// replica has applied everything that it received, whether it is connected
// to its primary, how many connections are open, how many the server
// allows, whether the server must have primary_conninfo, its block reads,
// whether it archives its write-ahead log, and, as its administrator, how
// long that log waits to be archived, and the oldest transaction ID age
// over every database, including templates.
func postgresqlQuestion(administrator bool) string {
	archiveWait := "NULL"
	if administrator {
		archiveWait = postgresqlArchiveWait
	}
	return postgresqlState + ", " + postgresqlBlockReads + ", current_setting('archive_mode'), " + archiveWait + ", (SELECT max(age(datfrozenxid)) FROM pg_database)"
}

const postgresqlState = "SELECT pg_is_in_recovery(), " +
	"COALESCE(EXTRACT(EPOCH FROM now() - pg_last_xact_replay_timestamp())::text, ''), " +
	"COALESCE(pg_last_wal_receive_lsn() = pg_last_wal_replay_lsn(), false), " +
	"(SELECT count(*) FROM pg_stat_wal_receiver), " +
	// The connections that count against max_connections are those of
	// clients. A role that may read all statistics sees which those are.
	// Every other role sees only how many processes of any kind use a
	// database, which also counts the server's own workers and so reads a
	// little high.
	"(SELECT CASE WHEN pg_has_role(current_user, 'pg_read_all_stats', 'MEMBER') " +
	"THEN (SELECT count(*) FROM pg_stat_activity WHERE backend_type = 'client backend') " +
	"ELSE (SELECT sum(numbackends) FROM pg_stat_database) END), " +
	"current_setting('max_connections'), " +
	"CASE WHEN pg_is_in_recovery() THEN " +
	"(SELECT setting <> '' FROM pg_settings WHERE name = 'primary_conninfo') ELSE false END"

func (p *PostgreSQL) Run(ctx context.Context, r *CheckRun) Observation {
	password, problem := p.password()
	if problem != nil {
		return *problem
	}
	// Through the local socket, the server takes the operating system
	// account postgres for its own administrator. A check that states a
	// login needs no rights on this node, so its client runs as the
	// unprivileged account. psql never asks for a password: a server that
	// wants one that the check does not have is an error.
	account, environment := primitives.PostgreSQLAdministratorAccount, map[string]string{}
	arguments := []string{"psql", "--no-psqlrc", "--no-password", "--quiet", "--tuples-only", "--no-align", "--field-separator=|", "--set=ON_ERROR_STOP=1", "--dbname=postgres"}
	if p.LoginRole != "" {
		account = primitives.UnprivilegedAccount
	}
	if p.DatabaseServerHost != "" {
		arguments = append(arguments, "--host="+p.DatabaseServerHost)
		// psql takes how the connection is protected from its environment.
		// It refuses a server that does not keep to it: with verify-full
		// a certificate that is not valid for the host, with require a
		// server without encryption. An account with a Kerberos ticket
		// would get the encryption of Kerberos in the place of TLS, with
		// no certificate verified, so the check turns that off.
		environment["PGGSSENCMODE"] = "disable"
		switch p.ConnectionSecurity {
		case databaseVerifiedTLS:
			authorities, problem := p.trustedCertificateAuthorities(ctx)
			if problem != nil {
				return *problem
			}
			environment["PGSSLMODE"] = "verify-full"
			if p.DangerouslySkipServerCertificateCheck {
				// require otherwise uses ~/.postgresql/root.crt if it exists.
				// A path below /dev/null cannot exist, so no authority is loaded.
				environment["PGSSLMODE"], environment["PGSSLROOTCERT"] = "require", "/dev/null/mmwatchdog-no-certificate-check"
			} else {
				environment["PGSSLROOTCERT"] = authorities
			}
		default:
			environment["PGSSLMODE"] = "disable"
		}
	}
	if p.DatabaseServerPort != 0 {
		arguments = append(arguments, "--port="+strconv.Itoa(p.DatabaseServerPort))
	}
	if p.LoginRole != "" {
		arguments = append(arguments, "--username="+p.LoginRole)
	}
	if password != "" {
		environment["PGPASSWORD"] = password
	}
	ask := func(question string, fieldCount int) ([]string, *Observation) {
		answer, failed := askDatabaseClient(ctx, "PostgreSQL", account, append(slices.Clip(arguments), "--command="+question), environment)
		if failed != nil {
			return nil, failed
		}
		fields := strings.Split(strings.TrimSpace(answer), "|")
		if len(fields) != fieldCount {
			o := Result("unknown", primitives.DetailLine("psql gave an answer that MMWatchdog does not understand: "+primitives.Clip(answer, 200)))
			return nil, &o
		}
		return fields, nil
	}
	question := postgresqlQuestion(p.LoginRole == "")
	fields, failed := ask(question, 12)
	if failed != nil {
		return *failed
	}
	// A primary is behind no server, so it is 0 seconds behind. One check
	// with a limit then fits the primary and its replicas alike, and stays
	// right when a replica becomes the primary.
	o := Result("ok", "the PostgreSQL server answers; it is a primary")
	o.Measurements = map[string]Measurement{"replication_lag_seconds": {0, "seconds"}}
	unavailable := map[string]string{}
	if connections, err := strconv.ParseFloat(fields[4], 64); err == nil {
		if allowed, err := strconv.ParseFloat(fields[5], 64); err == nil && allowed > 0 {
			o.Measurements["connections_used_percent"] = Measurement{100 * connections / allowed, "percent"}
		}
	}
	if age, err := strconv.ParseFloat(fields[11], 64); err == nil {
		o.Measurements["transaction_ids_used_before_wraparound_percent"] = Measurement{100 * age / 2147483648, "percent"}
	}
	if fields[0] == "t" {
		// A replica that has applied everything that it received, and that
		// is connected to its primary, is not behind, however long ago the
		// primary made its last change. Without that connection, the age of
		// the last change that the replica applied is what MMWatchdog can know.
		lag, measured := 0.0, true
		if !(fields[2] == "t" && fields[3] != "0") {
			var err error
			lag, err = strconv.ParseFloat(fields[1], 64)
			measured = err == nil
		}
		if measured {
			o.Measurements["replication_lag_seconds"] = Measurement{max(lag, 0), "seconds"}
			o.Message = fmt.Sprintf("the PostgreSQL server answers; it is a replica, %s seconds behind its primary", primitives.FormatReading(max(lag, 0)))
		} else {
			delete(o.Measurements, "replication_lag_seconds")
			unavailable["replication_lag_seconds"] = "the PostgreSQL replica has not applied a change of its primary yet"
			o.Message = "the PostgreSQL server answers; it is a replica that has not applied a change of its primary yet"
		}
		if fields[3] == "0" && fields[6] == "" {
			o.Status = "unknown"
			o.Message = "the account of the check may not read primary_conninfo: grant pg_read_all_settings to the login role so MMWatchdog can check whether the replica must be connected to its primary"
		}
		if fields[3] == "0" && fields[6] == "t" {
			o.Status = "failed"
			o.Message += ", and it is not connected to its primary now"
		}
	}
	// A primary with archive_mode off has no segment that waits, and its 0
	// would hide that it archives nothing. A replica archives only with
	// archive_mode always; otherwise its primary does that, so it is 0
	// behind. A former primary keeps the segments that waited when it
	// became a replica, and no archiver runs on a replica to send them.
	switch {
	case p.LoginRole != "":
		unavailable["wal_archive_lag_seconds"] = "a check with login_role does not measure wal_archive_lag_seconds: PostgreSQL lists the write-ahead log that waits to be archived only to its administrators and to members of pg_monitor; check the server on its own node without login_role"
	case fields[0] == "t" && fields[9] != "always":
		o.Measurements["wal_archive_lag_seconds"] = Measurement{0, "seconds"}
	case fields[9] == "off":
		unavailable["wal_archive_lag_seconds"] = "archive_mode is off, so the PostgreSQL server archives no write-ahead log and wal_archive_lag_seconds cannot be measured"
	default:
		if wait, err := strconv.ParseFloat(fields[10], 64); err == nil {
			o.Measurements["wal_archive_lag_seconds"] = Measurement{max(wait, 0), "seconds"}
		}
	}
	// The cache hit ratio is the growth of the server's counters of block
	// reads since the run before. The first reading is the answer above;
	// the sample of a first run asks the same question once more.
	reads := fields[7:9]
	read := func() ([]uint64, error) {
		if reads == nil {
			again, failed := ask(question, 12)
			if failed != nil {
				return nil, errors.New(failed.Message)
			}
			reads = again[7:9]
		}
		counters := make([]uint64, len(reads))
		for i, field := range reads {
			var err error
			if counters[i], err = strconv.ParseUint(field, 10, 64); err != nil {
				return nil, errors.New("psql gave counters of block reads that MMWatchdog does not understand: " + primitives.Clip(strings.Join(reads, "|"), 200))
			}
		}
		reads = nil
		return counters, nil
	}
	before, now, window, unsampled := r.CounterReadings(ctx, read)
	if unsampled != "" {
		unavailable["cache_hit_percent"] = unsampled
		return withLimits(r.Check, o, unavailable)
	}
	hits, misses := float64(now[0]-before[0]), float64(now[1]-before[1]) //nolint:nilaway // CounterReadings returns nil samples only with a problem; the problem guard returns before indexing the counters.
	if hits+misses > 0 {
		o.Measurements["cache_hit_percent"] = Measurement{100 * hits / (hits + misses), "percent"}
	} else {
		unavailable["cache_hit_percent"] = fmt.Sprintf("the PostgreSQL server read no blocks in the last %.3fs, so cache_hit_percent cannot be measured", window.Seconds())
	}
	o.SampleWindow = window
	return withLimits(r.Check, o, unavailable)
}

// postgresqlDescription is what "mmwatchdog describe-module postgresql" shows, and what
// validation knows about the module's inputs.
var postgresqlDescription = ModuleDescription{
	Summary: "Asks a PostgreSQL server a question through its client program, psql, and measures how far a replica is behind, how many connections are in use, how many block reads its shared buffers served, how long its write-ahead log waits to be archived, and how far its transaction IDs are toward wraparound. Without inputs, the check asks the server on this node.",
	UsedAs:  UsedAsCheck,
	Inputs:  databaseServerInputs("PostgreSQL", "login_role", "as the operating system account postgres, which the server takes for its own administrator without a password"),
	Measurements: []ModuleMeasurement{
		{Name: "replication_lag_seconds", Unit: "seconds", Description: "How far a replica is behind its primary. A primary is 0 seconds behind, so one check with a limit fits the primary and its replicas."},
		{Name: "connections_used_percent", Unit: "percent", Description: "The open connections as a share of the connections that the server allows."},
		{Name: "cache_hit_percent", Unit: "percent", Description: "The share of the blocks that the server read since the run before that its shared buffers held, 0 to 100. A time in which the server read no block has no such measurement."},
		{Name: "wal_archive_lag_seconds", Unit: "seconds", Description: "How long the oldest finished segment of the write-ahead log has waited to be archived, 0 when none waits. Measured by a check without login_role, on a server that archives: a primary with archive_mode off has no such measurement, and a replica is 0 unless its archive_mode is always."},
		{Name: "transaction_ids_used_before_wraparound_percent", Unit: "percent", Description: "How far the oldest transaction ID of the server's databases is toward the limit at which PostgreSQL refuses every write until a vacuum finishes, 0 to 100. A healthy server reads about 5 to 10; higher means less room remains."},
	},
	Examples: []ModuleExample{
		{
			Description:   "The PostgreSQL server of this node answers.",
			Configuration: "postgresql: {}\nalert_severity: sos\n",
		},
		{
			Description:   "The replica on this node is less than 60 seconds behind its primary.",
			Configuration: "postgresql: {}\nfail_when_measurement_above:\n  replication_lag_seconds: 60\nalert_severity: warn\n",
		},
		{
			Description:   "The database server on another machine answers a login over an encrypted connection, and its certificate is valid for its name.",
			Configuration: "postgresql:\n  database_server_port: 5432\n  database_server_host: db-1.internal.example.com\n  connection_security: verified_tls\n  login_role: mmwatchdog_monitoring\n  login_password_file: /etc/mmwatchdog/secrets/postgresql-mmwatchdog-monitoring\nalert_severity: sos\n",
		},
		{
			Description:   "The oldest transaction ID of the server on this node is less than half way to the limit at which PostgreSQL refuses writes.",
			Configuration: "postgresql: {}\nfail_when_measurement_above:\n  transaction_ids_used_before_wraparound_percent: 50\nalert_severity: warn\n",
		},
	},
}
