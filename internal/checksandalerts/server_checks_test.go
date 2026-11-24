package checksandalerts_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// The tests of the checks of common server software and of the machine
// itself: PostgreSQL, MySQL, a Docker container, a SQLite database,
// security updates, a required reboot, a software RAID array, and the
// temperature. The rules are in docs/changing-mmwatchdog/design/checks.md under "Protocols and
// common server software" and "Security updates, required reboots, software
// RAID, and temperature". The client programs are stand-ins on PATH.

// asThisAccount lets the core modules run their programs as the account of
// the test, in place of mmwatchdog, postgres, and root.
func asThisAccount(t *testing.T) (ranAs func() []string) {
	t.Helper()
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	var guard sync.Mutex
	real := primitives.LookUpUserAccount
	primitives.LookUpUserAccount = func(name string) (*user.User, error) {
		guard.Lock()
		names = append(names, name)
		guard.Unlock()
		account := *current
		return &account, nil
	}
	t.Cleanup(func() { primitives.LookUpUserAccount = real })
	// ranAs returns the accounts that MMWatchdog ran a program as since the call
	// before, in the order of the runs.
	return func() []string {
		guard.Lock()
		defer guard.Unlock()
		asked := names
		names = nil
		return asked
	}
}

// nodeFiles puts files in the place of the files of the node's operating
// system for one test, by their absolute paths. A name that ends with a
// slash is an empty directory. It returns the directory that holds them.
func nodeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		path := filepath.Join(root, name)
		if strings.HasSuffix(name, "/") {
			if err := os.MkdirAll(path, 0755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	real := primitives.NodeFilesRoot
	primitives.NodeFilesRoot = root
	t.Cleanup(func() { primitives.NodeFilesRoot = real })
	return root
}

// clientProgram puts a stand-in for the client program of a server on PATH.
// It writes the file answer and ends with the exit code in the file
// exit_code, and it records in the file asked its arguments, a line
// "standard input: " with what it read on its standard input when that is
// not empty, and the named variables of its environment. The three paths
// are returned. Asked for its version, it writes the file answer.version
// and records nothing.
func clientProgram(t *testing.T, name string, variables ...string) (answer, exitCode, asked string) {
	t.Helper()
	return clientProgramWithLink(t, name, "", variables...)
}

// clientProgramWithLink is clientProgram for a program that a link also
// names, such as dnf5 and the link dnf on Fedora: the link runs the stand-in.
func clientProgramWithLink(t *testing.T, name, link string, variables ...string) (answer, exitCode, asked string) {
	t.Helper()
	directory := t.TempDir()
	answer, exitCode, asked = filepath.Join(directory, "answer"), filepath.Join(directory, "exit_code"), filepath.Join(directory, "asked")
	script := "if [ \"$1\" = --version ] || [ \"$2\" = --version ]; then cat " + testhelpers.ShellQuote(answer) + ".version 2>/dev/null; exit 0; fi\n"
	script += "printf '%s\\n' \"$@\" > " + testhelpers.ShellQuote(asked) + "\n"
	script += "input=$(cat); if [ -n \"$input\" ]; then printf 'standard input: %s\\n' \"$input\" >> " + testhelpers.ShellQuote(asked) + "; fi\n"
	for _, variable := range variables {
		script += "printf '%s\\n' \"" + variable + "=$" + variable + "\" >> " + testhelpers.ShellQuote(asked) + "\n"
	}
	script += "cat " + testhelpers.ShellQuote(answer) + "\nif [ -e " + testhelpers.ShellQuote(answer) + ".stderr ]; then cat " + testhelpers.ShellQuote(answer) + ".stderr >&2; fi\nexit $(cat " + testhelpers.ShellQuote(exitCode) + ")"
	os.WriteFile(answer, nil, 0644)
	os.WriteFile(exitCode, []byte("0"), 0644)
	if link == "" {
		fakeProgram(t, name, script)
		return answer, exitCode, asked
	}
	// A stand-in is a link to a launcher that runs the script of the name
	// that started it, so the link needs the script too.
	programs := t.TempDir()
	testhelpers.WriteStandInProgram(t, filepath.Join(programs, name), "#!/bin/sh\n"+script+"\n")
	testhelpers.WriteStandInProgram(t, filepath.Join(programs, link), "#!/bin/sh\n"+script+"\n")
	if err := os.Remove(filepath.Join(programs, link)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(name, filepath.Join(programs, link)); err != nil {
		t.Fatal(err)
	}
	testhelpers.PutProgramsFirst(t, programs)
	return answer, exitCode, asked
}

// A postgresql check asks through psql. Without inputs it uses the local
// socket as the server's own administrator; with a host it logs in with a
// password, which travels in the environment and never as an argument.
func TestPostgreSQLCheck(t *testing.T) {
	ranAs := asThisAccount(t)
	answer, exitCode, asked := clientProgram(t, "psql", "PGPASSWORD", "PGSSLMODE", "PGSSLROOTCERT")
	says := func(text string) { os.WriteFile(answer, []byte(text), 0644) }
	arguments := func() string {
		data, _ := os.ReadFile(asked)
		return string(data)
	}
	observe := func(postgresql checksandalerts.PostgreSQL, check checksandalerts.Check) checksandalerts.Observation {
		check.Inputs = &postgresql
		return withinTimeLimit(t, 5*time.Second, check)
	}
	// A primary with 12 of 100 connections in use. It is behind no server.
	says("f||f|0|12|100|t|990|10|on|0|1073741824\n")
	o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{})
	if o.Status != "ok" || o.Message != "the PostgreSQL server answers; it is a primary" || !reflect.DeepEqual(o.Measurements, map[string]checksandalerts.Measurement{"connections_used_percent": {Value: 12, Unit: "percent"}, "replication_lag_seconds": {Value: 0, Unit: "seconds"}, "wal_archive_lag_seconds": {Value: 0, Unit: "seconds"}, "transaction_ids_used_before_wraparound_percent": {Value: 100.0 * 1073741824 / 2147483648, Unit: "percent"}}) {
		t.Fatal(o)
	}
	if got := arguments(); !strings.HasPrefix(got, "--no-psqlrc\n--no-password\n--quiet\n--tuples-only\n--no-align\n--field-separator=|\n--set=ON_ERROR_STOP=1\n--dbname=postgres\n--command=SELECT pg_is_in_recovery(), ") || !strings.HasSuffix(got, "\nPGPASSWORD=\nPGSSLMODE=\nPGSSLROOTCERT=\n") ||
		strings.Contains(got, "--host") || strings.Contains(got, "--login_user") {
		t.Fatalf("%q", got)
	}
	// Without a login, the client runs as the server's own administrator,
	// which the server takes through its local socket. A first run asks
	// twice, for the sample of its cache hit ratio.
	if got := ranAs(); !reflect.DeepEqual(got, []string{"postgres", "postgres"}) {
		t.Fatal(got)
	}
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"connections_used_percent": 10}}); o.Status != "failed" || o.Message != "connections_used_percent is 12 (fails above 10); the PostgreSQL server answers; it is a primary" {
		t.Fatal(o)
	}
	// An age of 2^30 has used half of the 2^31 transaction IDs.
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"transaction_ids_used_before_wraparound_percent": 40}}); o.Status != "failed" || o.Message != "transaction_ids_used_before_wraparound_percent is 50 (fails above 40); the PostgreSQL server answers; it is a primary" {
		t.Fatal(o)
	}
	// An unreadable age is omitted without changing the check status.
	says("f||f|0|12|100|t|990|10|on|0|not an age\n")
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	} else if _, present := o.Measurements["transaction_ids_used_before_wraparound_percent"]; present {
		t.Fatal(o)
	}
	says("f||f|0|12|100|t|990|10|on|0|1073741824\n")
	// One check with a limit on the lag fits the primary and its replicas:
	// the primary is 0 seconds behind.
	lag := checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"replication_lag_seconds": 60}}
	if o := observe(checksandalerts.PostgreSQL{}, lag); o.Status != "ok" || o.Message != "replication_lag_seconds is 0 (fails above 60); the PostgreSQL server answers; it is a primary" {
		t.Fatal(o)
	}
	// A replica that has applied everything and is connected is not
	// behind, however old the primary's last change is.
	says("t|3600.5|t|1|3|100|t|990|10|on|0|1073741824\n")
	if o := observe(checksandalerts.PostgreSQL{}, lag); o.Status != "ok" || o.Measurements["replication_lag_seconds"] != (checksandalerts.Measurement{Value: 0, Unit: "seconds"}) ||
		o.Message != "replication_lag_seconds is 0 (fails above 60); the PostgreSQL server answers; it is a replica, 0 seconds behind its primary" {
		t.Fatal(o)
	}
	// A replica that still has changes to apply.
	says("t|75.25|f|1|3|100|t|990|10|on|0|1073741824\n")
	if o := observe(checksandalerts.PostgreSQL{}, lag); o.Status != "failed" || o.Measurements["replication_lag_seconds"].Value != 75.25 || !strings.HasSuffix(o.Message, "it is a replica, 75.25 seconds behind its primary") {
		t.Fatal(o)
	}
	// A replica that lost its primary falls behind from then on, also when
	// it has applied all that it received.
	says("t|900|t|0|3|100|t|990|10|on|0|1073741824\n")
	if o := observe(checksandalerts.PostgreSQL{}, lag); o.Status != "failed" || o.Measurements["replication_lag_seconds"].Value != 900 || !strings.HasSuffix(o.Message, "900 seconds behind its primary, and it is not connected to its primary now") {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "failed" || !strings.HasSuffix(o.Message, "and it is not connected to its primary now") {
		t.Fatal(o)
	}
	says("t|900|t|0|3|100|f|990|10|on|0|1073741824\n")
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	}
	says("t|900|t|0|3|100||990|10|on|0|1073741824\n")
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "unknown" || !strings.Contains(o.Message, "grant pg_read_all_settings to the login role") {
		t.Fatal(o)
	}
	// A new replica that has applied nothing yet has no number, and never
	// the 0 of a primary.
	says("t||f|1|3|100|t|990|10|on|0|1073741824\n")
	if o := observe(checksandalerts.PostgreSQL{}, lag); o.Status != "unknown" || o.Message != "the PostgreSQL replica has not applied a change of its primary yet" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "ok" || o.Message != "the PostgreSQL server answers; it is a replica that has not applied a change of its primary yet" || !reflect.DeepEqual(o.Measurements, map[string]checksandalerts.Measurement{"connections_used_percent": {Value: 3, Unit: "percent"}, "wal_archive_lag_seconds": {Value: 0, Unit: "seconds"}, "transaction_ids_used_before_wraparound_percent": {Value: 100.0 * 1073741824 / 2147483648, Unit: "percent"}}) {
		t.Fatal(o)
	}
	// A server on another machine: a login with a password.
	passwordFile := filepath.Join(t.TempDir(), "password")
	os.WriteFile(passwordFile, []byte("a secret password\n"), 0600)
	says("f||f|0|1|100|t|990|10|on|0|1073741824\n")
	remote := checksandalerts.PostgreSQL{DatabaseServer: checksandalerts.DatabaseServer{DatabaseServerHost: "db-1.internal.example.com", DatabaseServerPort: 5433, ConnectionSecurity: "unencrypted", LoginPasswordFile: passwordFile}, LoginRole: "mmwatchdog_monitoring"}
	if o := observe(remote, checksandalerts.Check{}); o.Status != "ok" || o.Measurements["transaction_ids_used_before_wraparound_percent"] != (checksandalerts.Measurement{Value: 100.0 * 1073741824 / 2147483648, Unit: "percent"}) {
		t.Fatal(o)
	}
	ranAs()
	if o := observe(remote, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	}
	if got := arguments(); !strings.Contains(got, "\n--host=db-1.internal.example.com\n--port=5433\n--username=mmwatchdog_monitoring\n--command=") || !strings.HasSuffix(got, "\nPGPASSWORD=a secret password\nPGSSLMODE=disable\nPGSSLROOTCERT=\n") || strings.Count(got, "a secret password") != 1 {
		t.Fatalf("%q", got)
	}
	// A check with a login needs no rights on this node: its client runs
	// as the unprivileged account, for a server on another machine and for
	// the server on this node.
	if got := ranAs(); !reflect.DeepEqual(got, []string{"mmwatchdog", "mmwatchdog"}) {
		t.Fatal(got)
	}
	if o := observe(checksandalerts.PostgreSQL{DatabaseServer: checksandalerts.DatabaseServer{LoginPasswordFile: passwordFile}, LoginRole: "mmwatchdog_monitoring"}, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	}
	if got := ranAs(); !reflect.DeepEqual(got, []string{"mmwatchdog", "mmwatchdog"}) {
		t.Fatal(got)
	}
	// The server refuses, or is not there: the first line of what psql says.
	os.WriteFile(exitCode, []byte("2"), 0644)
	os.WriteFile(answer+".stderr", []byte("psql: error: connection to server on socket \"/var/run/postgresql/.s.PGSQL.5432\" failed: No such file or directory\n\tIs the server running locally and accepting connections on that socket?\n"), 0644)
	says("")
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != `the PostgreSQL server did not answer the check: psql: error: connection to server on socket "/var/run/postgresql/.s.PGSQL.5432" failed: No such file or directory` {
		t.Fatal(o)
	}
	// psql puts the reason at the end of its line, behind the name and the
	// address of the server. A line with long names stays whole, and a
	// line that is too long for a message keeps its start and its end.
	wholeLine := `psql: error: connection to server at "database-of-the-orders.internal.example.com" (2001:db8:85a3::8a2e:370:7334), port 5432 failed: server certificate for "database.internal.example.com" does not match host name "database-of-the-orders.internal.example.com"`
	os.WriteFile(answer+".stderr", []byte(wholeLine+"\n"), 0644)
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != "the PostgreSQL server did not answer the check: "+wholeLine {
		t.Fatal(o)
	}
	os.WriteFile(answer+".stderr", []byte(`psql: error: connection to server at "`+strings.Repeat("a-long-name.", 30)+`example.com" (10.0.0.5), port 5432 failed: FATAL:  password authentication failed for user "mmwatchdog_monitoring"`+"\n"), 0644)
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "failed" || !strings.HasPrefix(o.Message, `the PostgreSQL server did not answer the check: psql: error: connection to server at "a-long-name.a-long-name.`) ||
		!strings.HasSuffix(o.Message, `a-long-name.example.com" (10.0.0.5), port 5432 failed: FATAL:  password authentication failed for user "mmwatchdog_monitoring"`) || !strings.Contains(o.Message, "a-long-name.a- ... e.a-long-name.") || len(o.Message) != len("the PostgreSQL server did not answer the check: ")+100+len(" ... ")+200 {
		t.Fatal(len(o.Message), o)
	}
	os.Remove(answer + ".stderr")
	os.WriteFile(exitCode, []byte("0"), 0644)
	says("something else\n")
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "unknown" || !strings.HasPrefix(o.Message, "psql gave an answer that MMWatchdog does not understand: ") {
		t.Fatal(o)
	}
	// A password file that cannot be read, and a node without psql.
	remote.LoginPasswordFile = filepath.Join(t.TempDir(), "missing")
	if o := observe(remote, checksandalerts.Check{}); o.Status != "unknown" {
		t.Fatal(o)
	}
	if problem := remote.ValidateOnNode(); problem == nil || problem.Path != "login_password_file" {
		t.Fatal(problem)
	}
	testhelpers.SetProgramEnvironment(t, "PATH", t.TempDir())
	if o := observe(checksandalerts.PostgreSQL{}, checksandalerts.Check{}); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot run psql, the client program of PostgreSQL: ") || !strings.HasSuffix(o.Message, "The check needs that program on this node") {
		t.Fatal(o)
	}
}

// A postgresql check measures the share of the blocks read since the run
// before that the server's shared buffers held, and how long the oldest
// finished segment of the write-ahead log has waited to be archived.
func TestPostgreSQLCheckMeasuresCacheHitsAndTheArchiveWait(t *testing.T) {
	asThisAccount(t)
	answer, _, _ := clientProgram(t, "psql")
	says := func(text string) { os.WriteFile(answer, []byte(text), 0644) }
	timing := checksandalerts.Timing{CheckEvery: "1m", TimeLimitPerRun: "10s"}
	run := checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: &checksandalerts.PostgreSQL{},
		FailWhenMeasurementBelow: map[string]float64{"cache_hit_percent": 99}, FailWhenMeasurementAbove: map[string]float64{"wal_archive_lag_seconds": 300}}, timing)
	observe := func() checksandalerts.Observation { return checksandalerts.RunCheck(testhelpers.TestContext(t), run) }
	// The first run samples the counters twice. A server that read no
	// block in between has no ratio, never one of 0 or 100.
	says("f||f|0|1|100|f|1000|10|on|0|1073741824\n")
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "the PostgreSQL server read no blocks in the last ") || !strings.HasSuffix(o.Message, "s, so cache_hit_percent cannot be measured") {
		t.Fatal(o)
	}
	// 990 of the 1,000 blocks read since the run before were in the
	// buffers: 99 percent, which is not below the limit.
	says("f||f|0|1|100|f|1990|20|on|0|1073741824\n")
	o := observe()
	if o.Status != "ok" || o.Measurements["cache_hit_percent"] != (checksandalerts.Measurement{Value: 99, Unit: "percent"}) || o.Measurements["wal_archive_lag_seconds"] != (checksandalerts.Measurement{Value: 0, Unit: "seconds"}) ||
		!strings.Contains(o.Message, "cache_hit_percent is 99 (fails below 99)") || !strings.HasSuffix(o.Message, "the PostgreSQL server answers; it is a primary") || o.SampleWindow <= 0 {
		t.Fatal(o)
	}
	// Half of the next 1,000 came from outside the buffers, and a segment
	// has waited 412.5 seconds to be archived.
	says("f||f|0|1|100|f|2490|520|on|412.5|1073741824\n")
	if o := observe(); o.Status != "failed" || o.Measurements["cache_hit_percent"].Value != 50 || o.Measurements["wal_archive_lag_seconds"].Value != 412.5 ||
		!strings.Contains(o.Message, "cache_hit_percent is 50 (fails below 99)") || !strings.Contains(o.Message, "wal_archive_lag_seconds is 412.5 (fails above 300)") {
		t.Fatal(o)
	}
	// A primary with archive_mode off archives nothing: no segment waits,
	// and a 0 would hide that.
	says("f||f|0|1|100|f|3490|520|off|0|1073741824\n")
	if o := observe(); o.Status != "unknown" || o.Message != "archive_mode is off, so the PostgreSQL server archives no write-ahead log and wal_archive_lag_seconds cannot be measured" {
		t.Fatal(o)
	}
	// A replica archives nothing, as its primary does that: it is 0
	// behind, so the check stays right when it is made the primary.
	says("t|0|t|1|1|100|t|4490|520|off|0|1073741824\n")
	if o := observe(); o.Status != "ok" || o.Measurements["wal_archive_lag_seconds"].Value != 0 {
		t.Fatal(o)
	}
	// A former primary keeps the segments that waited when it became a
	// replica, and with archive_mode on no archiver runs to send them.
	says("t|0|t|1|1|100|t|5490|520|on|2086|1073741824\n")
	if o := observe(); o.Status != "ok" || o.Measurements["wal_archive_lag_seconds"].Value != 0 {
		t.Fatal(o)
	}
	// With archive_mode always, a replica archives too, and its own wait
	// counts.
	says("t|0|t|1|1|100|t|6490|520|always|412.5|1073741824\n")
	if o := observe(); o.Status != "failed" || o.Measurements["wal_archive_lag_seconds"].Value != 412.5 || !strings.Contains(o.Message, "wal_archive_lag_seconds is 412.5 (fails above 300)") {
		t.Fatal(o)
	}
	// PostgreSQL lists the segments that wait only to its administrators
	// and to pg_monitor, so a check that logs in does not ask for them.
	passwordFile := filepath.Join(t.TempDir(), "password")
	os.WriteFile(passwordFile, []byte("a secret password\n"), 0600)
	says("f||f|0|1|100|f|5490|520|on||1073741824\n")
	login := checksandalerts.Check{Inputs: &checksandalerts.PostgreSQL{DatabaseServer: checksandalerts.DatabaseServer{LoginPasswordFile: passwordFile}, LoginRole: "mmwatchdog_monitoring"},
		FailWhenMeasurementAbove: map[string]float64{"wal_archive_lag_seconds": 300}}
	if o := checksandalerts.RunCheck(testhelpers.TestContext(t), checksandalerts.NewCheckRun(login, timing)); o.Status != "unknown" || !strings.HasPrefix(o.Message, "a check with login_role does not measure wal_archive_lag_seconds: ") {
		t.Fatal(o)
	}
}

// A pgbackrest check reads "pgbackrest info --output=json" of its stanza,
// as the account of the PostgreSQL server, and measures how many hours ago
// the latest backup without errors finished. The answers are those of
// pgBackRest 2.55.1, with the times of the backups moved to the test's.
func TestPgBackRestCheck(t *testing.T) {
	ranAs := asThisAccount(t)
	answer, exitCode, asked := clientProgram(t, "pgbackrest")
	hoursAgo := func(hours float64) int64 { return time.Now().Add(-time.Duration(hours * float64(time.Hour))).Unix() }
	says := func(lockHeld bool, backups ...string) {
		os.WriteFile(answer, []byte(`[{"archive":[{"database":{"id":1,"repo-key":1},"id":"17-1","max":"000000010000000000000005","min":"000000010000000000000001"}],"backup":[`+strings.Join(backups, ",")+
			`],"cipher":"none","db":[{"id":1,"repo-key":1,"system-id":7701269603235061331,"version":"17"}],"name":"main","repo":[{"cipher":"none","key":1,"status":{"code":0,"message":"ok"}}],"status":{"code":0,"lock":{"backup":{"held":`+fmt.Sprint(lockHeld)+`}},"message":"ok"}}]`), 0644)
	}
	backup := func(label, kind string, stop int64, damaged bool) string {
		return fmt.Sprintf(`{"archive":{"start":"000000010000000000000002","stop":"000000010000000000000003"},"backrest":{"format":5,"version":"2.55.1"},"database":{"id":1,"repo-key":1},"error":%t,"info":{"delta":23161246,"repository":{"delta":2979635,"size":2979635},"size":23161246},"label":"%s","lsn":{"start":"0/2000028","stop":"0/3000050"},"prior":null,"reference":null,"timestamp":{"start":%d,"stop":%d},"type":"%s"}`, damaged, label, stop-12, stop, kind)
	}
	stanza := &checksandalerts.PgBackRest{Stanza: "main"}
	daily := checksandalerts.Check{Inputs: stanza, FailWhenMeasurementAbove: map[string]float64{"latest_backup_age_hours": 26}}
	observe := func(check checksandalerts.Check) checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, check)
	}
	// A full backup and a newer differential one: the newer one counts,
	// whatever its type.
	says(false, backup("20261027-085929F", "full", hoursAgo(27), false), backup("20261027-085929F_20261027-085941D", "diff", hoursAgo(3), false))
	o := observe(daily)
	if o.Status != "ok" || o.Measurements["latest_backup_age_hours"].Unit != "hours" || o.Measurements["latest_backup_age_hours"].Value < 2.99 || o.Measurements["latest_backup_age_hours"].Value > 3.01 ||
		o.Message != "latest_backup_age_hours is 3 (fails above 26); the latest backup of stanza main finished 3 hours ago: 20261027-085929F_20261027-085941D, a diff backup" {
		t.Fatal(o)
	}
	if data, _ := os.ReadFile(asked); string(data) != "--stanza=main\n--output=json\ninfo\n" {
		t.Fatalf("%q", data)
	}
	// pgBackRest's settings hold the keys of its repository, which the
	// account of the PostgreSQL server reads.
	if got := ranAs(); !reflect.DeepEqual(got, []string{"postgres"}) {
		t.Fatal(got)
	}
	// A missed daily backup.
	says(false, backup("20261026-020000F", "full", hoursAgo(30), false))
	if o := observe(daily); o.Status != "failed" || !strings.HasPrefix(o.Message, "latest_backup_age_hours is 30 (fails above 26); ") {
		t.Fatal(o)
	}
	// A newer backup that found damaged pages does not count, and the
	// message names it. A running backup is named too.
	says(true, backup("20261026-020000F", "full", hoursAgo(30), false), backup("20261026-020000F_20261027-020000D", "diff", hoursAgo(6), true))
	if o := observe(daily); o.Status != "failed" || !strings.HasSuffix(o.Message, "the latest backup of stanza main finished 30 hours ago: 20261026-020000F, a full backup; pgBackRest found damaged pages in the newer 20261026-020000F_20261027-020000D; a backup is running now") {
		t.Fatal(o)
	}
	says(false, backup("20261026-020000F", "full", hoursAgo(30), true))
	if o := observe(checksandalerts.Check{Inputs: stanza}); o.Status != "failed" || o.Message != "stanza main has no backup without errors; pgBackRest found damaged pages in 20261026-020000F" {
		t.Fatal(o)
	}
	// A stanza that pgBackRest reports without a problem and without a
	// backup names no damage.
	says(false)
	if o := observe(checksandalerts.Check{Inputs: stanza}); o.Status != "failed" || o.Message != "stanza main has no backup" {
		t.Fatal(o)
	}
	// pgBackRest exits with 0 when the stanza has a problem. The reason of
	// a repository's problem is in the repository's status, on its first
	// line; the stanza's status then says only "other".
	os.WriteFile(answer, []byte(`[{"archive":[],"backup":[],"cipher":"none","db":[],"name":"main","repo":[{"cipher":"none","key":1,"status":{"code":1,"message":"missing stanza path"}}],"status":{"code":1,"lock":{"backup":{"held":false}},"message":"missing stanza path"}}]`), 0644)
	if o := observe(checksandalerts.Check{Inputs: stanza}); o.Status != "failed" || o.Message != "pgBackRest reports a problem with stanza main: repo1: missing stanza path" {
		t.Fatal(o)
	}
	os.WriteFile(answer, []byte(`[{"archive":[],"backup":[],"cipher":"none","db":[],"name":"main","repo":[{"cipher":"none","key":1,"status":{"code":99,"message":"[FileOpenError] unable to load info file '/var/lib/pgbackrest/archive/main/archive.info' or '/var/lib/pgbackrest/archive/main/archive.info.copy':\nFileOpenError: unable to open file '/var/lib/pgbackrest/archive/main/archive.info' for read: [13] Permission denied"}}],"status":{"code":99,"lock":{"backup":{"held":false}},"message":"other"}}]`), 0644)
	if o := observe(daily); o.Status != "failed" || o.Message != "pgBackRest reports a problem with stanza main: repo1: [FileOpenError] unable to load info file '/var/lib/pgbackrest/archive/main/archive.info' or '/var/lib/pgbackrest/archive/main/archive.info.copy':" {
		t.Fatal(o)
	}
	os.WriteFile(answer, []byte(`[{"archive":[],"backup":[],"cipher":"none","db":[],"name":"main","repo":[],"status":{"code":2,"lock":{"backup":{"held":false}},"message":"no valid backups"}}]`), 0644)
	if o := observe(daily); o.Status != "failed" || o.Message != "pgBackRest reports a problem with stanza main: no valid backups" {
		t.Fatal(o)
	}
	// pgBackRest itself fails, and an answer that MMWatchdog does not
	// understand.
	os.WriteFile(exitCode, []byte("37"), 0644)
	os.WriteFile(answer+".stderr", []byte("ERROR: [037]: info command requires option: stanza\n"), 0644)
	os.WriteFile(answer, nil, 0644)
	if o := observe(daily); o.Status != "failed" || o.Message != "pgBackRest did not answer the check: ERROR: [037]: info command requires option: stanza" {
		t.Fatal(o)
	}
	os.Remove(answer + ".stderr")
	os.WriteFile(exitCode, []byte("0"), 0644)
	os.WriteFile(answer, []byte("[]"), 0644)
	if o := observe(daily); o.Status != "unknown" || o.Message != "pgbackrest gave an answer that MMWatchdog does not understand: []" {
		t.Fatal(o)
	}
	// The stanza is one word.
	for _, name := range []string{"", "two words"} {
		if problem := (&checksandalerts.PgBackRest{Stanza: name}).Validate(checksandalerts.InputValidation{}); problem == nil || problem.Path != "stanza" {
			t.Fatal(name, problem)
		}
	}
	testhelpers.SetProgramEnvironment(t, "PATH", t.TempDir())
	if o := observe(daily); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot run pgbackrest: ") || !strings.HasSuffix(o.Message, "The check needs that program on this node") {
		t.Fatal(o)
	}
}

// A mysql check asks through the client program of MySQL or MariaDB, and
// reads the status of a replica in the words of the server's release.
func TestMySQLCheck(t *testing.T) {
	ranAs := asThisAccount(t)
	nodeFiles(t, nil)
	answer, exitCode, asked := clientProgram(t, "mysql", "MYSQL_TEST_LOGIN_FILE", "MYSQL_PWD")
	says := func(text string) { os.WriteFile(answer, []byte(text), 0644) }
	arguments := func() string {
		data, _ := os.ReadFile(asked)
		return string(data)
	}
	observe := func(mysql checksandalerts.MySQL, check checksandalerts.Check) checksandalerts.Observation {
		check.Inputs = &mysql
		return withinTimeLimit(t, 5*time.Second, check)
	}
	const server = "*************************** 1. row ***************************\nmax_connections: 200\n*************************** 1. row ***************************\nVariable_name: Threads_connected\n        Value: 30\n"
	replica := func(io, sql, behind string) string {
		return server + "*************************** 1. row ***************************\n               Slave_IO_State: Waiting for master to send event\n             Slave_IO_Running: " + io +
			"\n            Slave_SQL_Running: " + sql + "\n        Seconds_Behind_Master: " + behind + "\n                Last_IO_Error: error connecting to master 'repl@db-1:3306'\n               Last_SQL_Error: \n"
	}
	says(server)
	o := observe(checksandalerts.MySQL{}, checksandalerts.Check{})
	if o.Status != "ok" || o.Message != "the MySQL server answers; it is no replica" || !reflect.DeepEqual(o.Measurements, map[string]checksandalerts.Measurement{"connections_used_percent": {Value: 15, Unit: "percent"}, "replication_lag_seconds": {Value: 0, Unit: "seconds"}}) {
		t.Fatal(o)
	}
	if got := arguments(); got != "--no-defaults\n--local-infile=0\n--batch\n--vertical\n--force\n--connect-timeout=10\n--protocol=SOCKET\nstandard input: "+checksandalerts.MySQLQuestion+"SHOW ALL REPLICAS STATUS\nMYSQL_TEST_LOGIN_FILE=/dev/null\nMYSQL_PWD=\n" {
		t.Fatalf("%q", got)
	}
	// Without a login, the client runs as root, which the server takes for
	// its own administrator through its local socket. The check first asks
	// the client program under the name mysql whose it is.
	if got := ranAs(); !reflect.DeepEqual(got, []string{"root", "root"}) {
		t.Fatal(got)
	}
	// One check with a limit on the lag fits a source and its replicas: a
	// server that is no replica is 0 seconds behind.
	lag := checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"replication_lag_seconds": 60}}
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "ok" || o.Message != "replication_lag_seconds is 0 (fails above 60); the MySQL server answers; it is no replica" {
		t.Fatal(o)
	}
	says(replica("Yes", "Yes", "7"))
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "ok" || o.Measurements["replication_lag_seconds"] != (checksandalerts.Measurement{Value: 7, Unit: "seconds"}) || !strings.HasSuffix(o.Message, "the MySQL server answers; it is a replica, 7 seconds behind its source") {
		t.Fatal(o)
	}
	says(replica("Yes", "Yes", "600"))
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "failed" {
		t.Fatal(o)
	}
	// A replica that does not receive changes fails the check by itself.
	says(replica("Connecting", "Yes", "NULL"))
	o = observe(checksandalerts.MySQL{}, checksandalerts.Check{})
	if o.Status != "failed" || o.Message != "the MySQL replica does not follow its source: it is not receiving changes (Connecting); it is applying changes" || !reflect.DeepEqual(o.Details, []string{"Last_IO_Error: error connecting to master 'repl@db-1:3306'"}) {
		t.Fatal(o)
	}
	// Such a replica has no number for how far it is behind, and never the
	// 0 of a server that is no replica.
	if !reflect.DeepEqual(o.Measurements, map[string]checksandalerts.Measurement{"connections_used_percent": {Value: 15, Unit: "percent"}}) {
		t.Fatal(o.Measurements)
	}
	// A replica that follows several sources has one row for each, which
	// the server calls a channel. The check fails when one of them does
	// not follow, and names it; the lag is that of the source that the
	// replica is the furthest behind.
	channel := func(name, io, behind string) string {
		return "*************************** 1. row ***************************\n             Replica_IO_Running: " + io + "\n            Replica_SQL_Running: Yes\n          Seconds_Behind_Source: " + behind +
			"\n                  Last_IO_Error: \n                 Last_SQL_Error: \n                   Channel_Name: " + name + "\n"
	}
	says(server + channel("orders", "Connecting", "NULL") + channel("billing", "Yes", "0"))
	if o := observe(checksandalerts.MySQL{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != "the MySQL replica does not follow its source of the channel orders: it is not receiving changes (Connecting); it is applying changes" {
		t.Fatal(o)
	}
	says(server + channel("orders", "Yes", "4") + channel("billing", "Yes", "90"))
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "failed" || o.Measurements["replication_lag_seconds"].Value != 90 || !strings.HasSuffix(o.Message, "it is a replica, 90 seconds behind the 2 sources that it follows") {
		t.Fatal(o)
	}
	// A member of a group replication has a channel of its own kind with
	// no thread that receives, by its design. It is no replica of a source,
	// and the check has no number for it: never the 0 of a server that
	// follows nothing.
	says(server + channel("group_replication_applier", "No", "4000"))
	if o := observe(checksandalerts.MySQL{}, checksandalerts.Check{}); o.Status != "ok" || o.Message != "the MySQL server answers; it is a member of a group replication" || !reflect.DeepEqual(o.Measurements, map[string]checksandalerts.Measurement{"connections_used_percent": {Value: 15, Unit: "percent"}}) {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "unknown" || o.Message != "the MySQL server is a member of a group replication, and this check has no number for how far such a member is behind" {
		t.Fatal(o)
	}
	// A MariaDB replica names each source that it follows, as a
	// connection, and keeps the older words.
	connection := func(name, io, behind string) string {
		return "*************************** 1. row ***************************\n               Connection_name: " + name + "\n              Slave_IO_Running: " + io + "\n             Slave_SQL_Running: Yes\n         Seconds_Behind_Master: " + behind +
			"\n                 Last_IO_Error: \n                Last_SQL_Error: \n"
	}
	says(server + connection("orders", "Connecting", "NULL"))
	if o := observe(checksandalerts.MySQL{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != "the MySQL replica does not follow its source of the connection orders: it is not receiving changes (Connecting); it is applying changes" {
		t.Fatal(o)
	}
	says(server + connection("orders", "Yes", "12") + connection("billing", "Yes", "3"))
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "ok" || o.Measurements["replication_lag_seconds"].Value != 12 || !strings.HasSuffix(o.Message, "it is a replica, 12 seconds behind the 2 sources that it follows") {
		t.Fatal(o)
	}
	says(replica("Yes", "No", "NULL"))
	if o := observe(checksandalerts.MySQL{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != "the MySQL replica does not follow its source: it is receiving changes; it is not applying changes (No)" {
		t.Fatal(o)
	}
	// A newer MySQL names the columns with other words.
	says(server + "*************************** 1. row ***************************\n           Replica_IO_Running: Yes\n          Replica_SQL_Running: Yes\n        Seconds_Behind_Source: 0\n")
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "ok" || o.Measurements["replication_lag_seconds"].Value != 0 {
		t.Fatal(o)
	}
	// An account that may not ask for the status of a replica: the check is
	// healthy, and only a limit on the lag is unknown.
	says(server)
	os.WriteFile(answer+".stderr", []byte("ERROR 1227 (42000) at line 4: Access denied; you need (at least one of) the REPLICA MONITOR privilege(s) for this operation\n"), 0644)
	os.WriteFile(exitCode, []byte("1"), 0644)
	if o := observe(checksandalerts.MySQL{}, checksandalerts.Check{}); o.Status != "ok" || o.Message != "the MySQL server answers" || !reflect.DeepEqual(o.Measurements, map[string]checksandalerts.Measurement{"connections_used_percent": {Value: 15, Unit: "percent"}}) {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "unknown" || !strings.HasPrefix(o.Message, "the account of the check may not ask for the status of a replica: it needs the privilege REPLICATION CLIENT") {
		t.Fatal(o)
	}
	// The server refuses the question about the replica for another
	// reason. MMWatchdog then does not know whether the server is a replica: the
	// check is healthy without a number, and a limit on the lag is unknown.
	os.WriteFile(answer+".stderr", []byte("ERROR 1142 (42000) at line 4: SHOW command denied to user 'mmwatchdog_monitoring'@'localhost'\n"), 0644)
	if o := observe(checksandalerts.MySQL{}, checksandalerts.Check{}); o.Status != "ok" || o.Message != "the MySQL server answers" || !reflect.DeepEqual(o.Measurements, map[string]checksandalerts.Measurement{"connections_used_percent": {Value: 15, Unit: "percent"}}) {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "unknown" || o.Message != "the MySQL server did not say whether it is a replica: ERROR 1142 (42000) at line 4: SHOW command denied to user 'mmwatchdog_monitoring'@'localhost'" {
		t.Fatal(o)
	}
	// InnoDB's history list length. MariaDB gives it to every account as
	// the status variable Innodb_history_list_length. MySQL has only
	// InnoDB's counter trx_rseg_history_len, which an account reads with
	// the privilege PROCESS.
	os.Remove(answer + ".stderr")
	os.WriteFile(exitCode, []byte("0"), 0644)
	history := checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"innodb_history_list_length": 1000000}}
	const mariaDBHistory = "*************************** 1. row ***************************\nVariable_name: Innodb_history_list_length\n        Value: 1200000\n"
	counter := func(count, status string) string {
		return "*************************** 1. row ***************************\n           NAME: trx_rseg_history_len\n      SUBSYSTEM: transaction\n          COUNT: " + count +
			"\n   TIME_ENABLED: 2026-10-16 08:00:00\n         STATUS: " + status + "\n        COMMENT: Length of the TRX_RSEG_HISTORY list\n"
	}
	says(server + mariaDBHistory)
	if o := observe(checksandalerts.MySQL{}, history); o.Status != "failed" || o.Message != "innodb_history_list_length is 1200000 (fails above 1000000); the MySQL server answers; it is no replica" || o.Measurements["innodb_history_list_length"] != (checksandalerts.Measurement{Value: 1200000, Unit: "transactions"}) {
		t.Fatal(o)
	}
	// A MariaDB account without PROCESS reads the status variable. MariaDB's
	// client writes the statement that the server refused in front of what
	// it says about it.
	refusedCounter := "--------------\nSELECT * FROM information_schema.INNODB_METRICS WHERE NAME = 'trx_rseg_history_len'\n--------------\n\nERROR 1227 (42000) at line 3: Access denied; you need (at least one of) the PROCESS privilege(s) for this operation\n"
	os.WriteFile(answer+".stderr", []byte(refusedCounter), 0644)
	os.WriteFile(exitCode, []byte("1"), 0644)
	if o := observe(checksandalerts.MySQL{}, history); o.Status != "failed" || o.Measurements["innodb_history_list_length"].Value != 1200000 || o.Measurements["replication_lag_seconds"].Unit != "seconds" {
		t.Fatal(o)
	}
	// MySQL's counter.
	os.Remove(answer + ".stderr")
	os.WriteFile(exitCode, []byte("0"), 0644)
	says(server + counter("300", "enabled"))
	if o := observe(checksandalerts.MySQL{}, history); o.Status != "ok" || o.Message != "innodb_history_list_length is 300 (fails above 1000000); the MySQL server answers; it is no replica" || o.Measurements["innodb_history_list_length"] != (checksandalerts.Measurement{Value: 300, Unit: "transactions"}) {
		t.Fatal(o)
	}
	// A counter that is turned off keeps its old number: no reading.
	says(server + counter("300", "disabled"))
	if o := observe(checksandalerts.MySQL{}, checksandalerts.Check{}); o.Status != "ok" || o.Measurements["innodb_history_list_length"].Unit != "" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.MySQL{}, history); o.Status != "unknown" || o.Message != "InnoDB's counter trx_rseg_history_len is turned off on the MySQL server, so its number is old: turn it on with SET GLOBAL innodb_monitor_enable = 'trx_rseg_history_len'" {
		t.Fatal(o)
	}
	// A MySQL account without PROCESS: no reading of the history, and the
	// statement after the refused one still answers, so a replica's lag is
	// read.
	says(server + replica("Yes", "Yes", "7"))
	os.WriteFile(answer+".stderr", []byte("ERROR 1227 (42000) at line 3: Access denied; you need (at least one of) the PROCESS privilege(s) for this operation\n"), 0644)
	os.WriteFile(exitCode, []byte("1"), 0644)
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "ok" || o.Measurements["replication_lag_seconds"].Value != 7 || o.Measurements["innodb_history_list_length"].Unit != "" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.MySQL{}, history); o.Status != "unknown" || o.Message != "the account of the check may not read the InnoDB history list length: it needs the privilege PROCESS" {
		t.Fatal(o)
	}
	// An account that may ask neither: each measurement says why it has no
	// reading.
	says(server)
	os.WriteFile(answer+".stderr", []byte("ERROR 1227 (42000) at line 3: Access denied; you need (at least one of) the PROCESS privilege(s) for this operation\nERROR 1227 (42000) at line 4: Access denied; you need (at least one of) the SUPER, REPLICATION CLIENT privilege(s) for this operation\n"), 0644)
	if o := observe(checksandalerts.MySQL{}, history); o.Status != "unknown" || o.Message != "the account of the check may not read the InnoDB history list length: it needs the privilege PROCESS" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.MySQL{}, lag); o.Status != "unknown" || !strings.HasPrefix(o.Message, "the account of the check may not ask for the status of a replica: ") {
		t.Fatal(o)
	}
	// The server refuses the question for the counter for another reason.
	os.WriteFile(answer+".stderr", []byte("ERROR 1109 (42S02) at line 3: Unknown table 'INNODB_METRICS' in information_schema\n"), 0644)
	if o := observe(checksandalerts.MySQL{}, history); o.Status != "unknown" || o.Message != "the MySQL server did not give the InnoDB history list length: ERROR 1109 (42S02) at line 3: Unknown table 'INNODB_METRICS' in information_schema" {
		t.Fatal(o)
	}

	// A server on another machine: a login with a password in the
	// environment of the client.
	os.Remove(answer + ".stderr")
	os.WriteFile(exitCode, []byte("0"), 0644)
	passwordFile := filepath.Join(t.TempDir(), "password")
	os.WriteFile(passwordFile, []byte("a secret password\n"), 0600)
	remote := checksandalerts.MySQL{DatabaseServer: checksandalerts.DatabaseServer{DatabaseServerHost: "db-1.internal.example.com", DatabaseServerPort: 3307, ConnectionSecurity: "unencrypted", LoginPasswordFile: passwordFile}, LoginUser: "mmwatchdog_monitoring"}
	if o := observe(remote, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	}
	ranAs()
	if o := observe(remote, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	}
	if got := arguments(); !strings.Contains(got, "\n--host=db-1.internal.example.com\n--protocol=TCP\n--port=3307\n--user=mmwatchdog_monitoring\n--ssl-mode=DISABLED\n--get-server-public-key\nstandard input: ") || !strings.HasSuffix(got, "\nMYSQL_PWD=a secret password\n") || strings.Count(got, "a secret password") != 1 {
		t.Fatalf("%q", got)
	}
	// A check with a login needs no rights on this node: its client runs
	// as the unprivileged account, also for the server on this node.
	if got := ranAs(); !reflect.DeepEqual(got, []string{"mmwatchdog", "mmwatchdog"}) {
		t.Fatal(got)
	}
	if o := observe(checksandalerts.MySQL{DatabaseServer: checksandalerts.DatabaseServer{LoginPasswordFile: passwordFile}, LoginUser: "mmwatchdog_monitoring"}, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	}
	if got := ranAs(); !reflect.DeepEqual(got, []string{"mmwatchdog", "mmwatchdog"}) {
		t.Fatal(got)
	}
	// The server refuses the login.
	says("")
	// A newer client writes a warning in front of what is wrong.
	os.WriteFile(answer+".stderr", []byte("WARNING: option --ssl-verify-server-cert is disabled, because of an insecure passwordless login.\nERROR 1045 (28000): Access denied for user 'mmwatchdog_monitoring'@'10.0.0.9' (using password: YES)\n"), 0644)
	os.WriteFile(exitCode, []byte("1"), 0644)
	if o := observe(remote, checksandalerts.Check{}); o.Status != "failed" || o.Message != "the MySQL server did not answer the check: ERROR 1045 (28000): Access denied for user 'mmwatchdog_monitoring'@'10.0.0.9' (using password: YES)" {
		t.Fatal(o)
	}

	loadChecks(t, map[string]string{
		"{postgresql: {}, alert_severity: sos}": "",
		"{postgresql: {}, fail_when_measurement_above: {replication_lag_seconds: 60, connections_used_percent: 80}, alert_severity: warn}":                                                                                                                                               "",
		"{postgresql: {database_server_port: 5433}, alert_severity: sos}":                                                                                                                                                                                                                "",
		"{postgresql: {database_server_port: 5432, database_server_host: db-1, connection_security: verified_tls, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}":                                                                        "",
		"{postgresql: {database_server_port: 5432, database_server_host: db-1, connection_security: verified_tls, database_server_certificate_authority_file: /etc/mmwatchdog/db-ca.pem, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}": "",
		"{postgresql: {database_server_port: 5432, database_server_host: db-1, connection_security: verified_tls, dangerously_skip_server_certificate_check: true, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}":                       "",
		"{postgresql: {database_server_port: 5432, database_server_host: db-1, connection_security: unencrypted, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}":                                                                         "",
		// A check of a server on another machine says how its connection is
		// protected, and no other check does.
		"{postgresql: {database_server_port: 5432, database_server_host: db-1, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}":                                "check_groups.c.postgresql.connection_security: required with database_server_host: write connection_security: verified_tls to encrypt the connection, or connection_security: unencrypted to send without encryption",
		"{postgresql: {database_server_port: 5432, database_server_host: db-1, connection_security: starttls, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}": "check_groups.c.postgresql.connection_security: required with database_server_host: write connection_security: verified_tls to encrypt the connection, or connection_security: unencrypted to send without encryption",
		"{postgresql: {connection_security: verified_tls}, alert_severity: sos}": "check_groups.c.postgresql.connection_security: has no use without database_server_host: the check uses the server's local socket on this node, which has no encryption and needs none",
		"{postgresql: {database_server_port: 5432, database_server_host: db-1, connection_security: unencrypted, database_server_certificate_authority_file: /etc/mmwatchdog/db-ca.pem, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}": "check_groups.c.postgresql.database_server_certificate_authority_file: has a use only with connection_security: verified_tls, which verifies the server's certificate",
		"{postgresql: {database_server_certificate_authority_file: /etc/mmwatchdog/db-ca.pem}, alert_severity: sos}":                                                                                                                                                     "check_groups.c.postgresql.database_server_certificate_authority_file: has no use without database_server_host: the check uses the server's local socket on this node",
		"{postgresql: {database_server_port: 5432, database_server_host: db-1, connection_security: verified_tls, database_server_certificate_authority_file: db-ca.pem, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}": "check_groups.c.postgresql.database_server_certificate_authority_file: must be absolute",
		"{postgresql: {login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}":                                                                                                                                                   "",
		"{postgresql: {database_server_port: 5432, database_server_host: db-1}, alert_severity: sos}":                                                                                                                                                                    "check_groups.c.postgresql.database_server_host: a check of a server on another machine logs in with a password: state login_role and login_password_file",
		"{postgresql: {database_server_port: 5432, database_server_host: db-1, login_role: mmwatchdog}, alert_severity: sos}":                                                                                                                                            "check_groups.c.postgresql.database_server_host: a check of a server on another machine logs in with a password",
		"{postgresql: {database_server_port: 5432, database_server_host: 'db-1:5432', login_role: mmwatchdog, login_password_file: /a}, alert_severity: sos}":                                                                                                            "check_groups.c.postgresql.database_server_host: expected the name or the address of the database server, without a port",
		"{postgresql: {database_server_port: 5432, database_server_host: '2001:db8::5', connection_security: unencrypted, login_role: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}":                                                "",
		"{postgresql: {database_server_port: 5432, database_server_host: '[2001:db8::5]:5432', login_role: mmwatchdog, login_password_file: /a}, alert_severity: sos}":                                                                                                   "check_groups.c.postgresql.database_server_host: expected the name or the address of the database server, without a port",
		"{postgresql: {database_server_port: 70000}, alert_severity: sos}":                                                                                                                                                                                               "check_groups.c.postgresql.database_server_port: expected a port number from 1 to 65535",
		"{postgresql: {login_password_file: /etc/mmwatchdog/secrets/pg}, alert_severity: sos}":                                                                                                                                                                           "check_groups.c.postgresql.login_role: required with login_password_file: the account that the check logs in as",
		"{postgresql: {login_role: mmwatchdog, login_password_file: secrets/pg}, alert_severity: sos}":                                                                                                                                                                   "check_groups.c.postgresql.login_password_file: must be absolute",
		"{postgresql: {login_user: mmwatchdog}, alert_severity: sos}":                                                                                                                                                                                                    `unknown field "login_user"`,
		"{postgresql: {}, fail_when_measurement_above: {lag: 60}, alert_severity: warn}":                                                                                                                                                                                 "check_groups.c.fail_when_measurement_above.lag: this check has no such measurement; it measures replication_lag_seconds, connections_used_percent",
		"{mysql: {}, alert_severity: sos}": "",
		"{mysql: {database_server_port: 3306, database_server_host: db-1, connection_security: verified_tls, login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/mysql}, fail_when_measurement_above: {connections_used_percent: 80}, alert_severity: sos}": "",
		"{mysql: {database_server_port: 3306, database_server_host: db-1, login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/mysql}, alert_severity: sos}":                                                                                                 "check_groups.c.mysql.connection_security: required with database_server_host: write connection_security: verified_tls to encrypt the connection, or connection_security: unencrypted to send without encryption",
		"{mysql: {connection_security: unencrypted}, alert_severity: sos}": "check_groups.c.mysql.connection_security: has no use without database_server_host",
		// The local socket of MySQL and of MariaDB has no port. That of
		// PostgreSQL has: its name holds the port.
		"{mysql: {database_server_port: 3307}, alert_severity: sos}": "check_groups.c.mysql.database_server_port: needs database_server_host: without it the check uses the server's local socket, which has no port. For a server on this node at a port, state database_server_host: 127.0.0.1",
		"{mysql: {database_server_host: 127.0.0.1, database_server_port: 3307, connection_security: unencrypted, login_user: mmwatchdog, login_password_file: /etc/mmwatchdog/secrets/mysql}, alert_severity: sos}": "",
		"{mysql: {database_server_port: 3306, database_server_host: db-1, login_password_file: /a}, alert_severity: sos}":                                                                                           "check_groups.c.mysql.database_server_host: a check of a server on another machine logs in with a password: state login_user and login_password_file",
		"{mysql: {login_role: mmwatchdog}, alert_severity: sos}": `unknown field "login_role"`,
		"{mysql: {}}": "check_groups.c.alert_severity: required: info, warn, or sos",
	})
}

// A check of a database server on another machine tells the client program
// how the connection is protected, in the words of that client program:
// psql through its environment, and the clients of MySQL and of MariaDB
// through arguments that differ.
func TestADatabaseCheckProtectsItsConnectionAsItStates(t *testing.T) {
	ranAs := asThisAccount(t)
	root := nodeFiles(t, nil)
	passwordFile := filepath.Join(t.TempDir(), "password")
	os.WriteFile(passwordFile, []byte("a secret password\n"), 0600)
	authorities, _ := testhelpers.TestCertificate(t, t.TempDir())
	skipCertificate := false
	server := func(security, authorityFile string) checksandalerts.DatabaseServer {
		return checksandalerts.DatabaseServer{DangerouslySkipServerCertificateCheck: skipCertificate, DatabaseServerHost: "db-1.internal.example.com", ConnectionSecurity: security, DatabaseServerCertificateAuthorityFile: authorityFile, LoginPasswordFile: passwordFile}
	}
	said := func(asked string) string {
		data, _ := os.ReadFile(asked)
		return string(data)
	}

	// PostgreSQL.
	answer, _, asked := clientProgram(t, "psql", "PGGSSENCMODE", "PGSSLMODE", "PGSSLROOTCERT")
	os.WriteFile(answer, []byte("f||f|0|1|100|t|990|10|on|0|1073741824\n"), 0644)
	postgresql := func(security, authorityFile string) checksandalerts.Observation {
		os.Remove(asked)
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.PostgreSQL{DatabaseServer: server(security, authorityFile), LoginRole: "mmwatchdog_monitoring"}})
	}
	// The check also turns off the encryption of Kerberos, which psql
	// would take in the place of TLS for an account that has a ticket.
	if o := postgresql("unencrypted", ""); o.Status != "ok" || !strings.HasSuffix(said(asked), "\nPGGSSENCMODE=disable\nPGSSLMODE=disable\nPGSSLROOTCERT=\n") {
		t.Fatal(o, said(asked))
	}
	// verified_tls with the check's own file of certificate authorities.
	if o := postgresql("verified_tls", authorities); o.Status != "ok" || !strings.HasSuffix(said(asked), "\nPGGSSENCMODE=disable\nPGSSLMODE=verify-full\nPGSSLROOTCERT="+authorities+"\n") {
		t.Fatal(o, said(asked))
	}
	// Without one, the check names the system's file, which psql does not
	// find by itself. A node without such a file cannot verify: the check
	// is unknown and starts no client, and validation on the node says
	// the same.
	noAuthorities := "verified_tls needs the certificate authorities that this node trusts, and this node has no file of them: MMWatchdog looked for /etc/ssl/certs/ca-certificates.crt, /etc/ssl/ca-bundle.pem, /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem. Install the distribution's package of certificate authorities, or state database_server_certificate_authority_file"
	if o := postgresql("verified_tls", ""); o.Status != "unknown" || o.Message != "connection_security: "+noAuthorities || said(asked) != "" {
		t.Fatal(o, said(asked))
	}
	withoutFile := server("verified_tls", "")
	if problem := withoutFile.ValidateOnNode(); problem == nil || problem.Path != "connection_security" || problem.Message != noAuthorities {
		t.Fatal(problem)
	}
	// openSUSE's file, on a node that has only that one.
	openSUSEFile := filepath.Join(root, "etc/ssl/ca-bundle.pem")
	os.MkdirAll(filepath.Dir(openSUSEFile), 0755)
	os.WriteFile(openSUSEFile, []byte("certificates of the system\n"), 0644)
	if o := postgresql("verified_tls", ""); o.Status != "ok" || !strings.HasSuffix(said(asked), "\nPGSSLMODE=verify-full\nPGSSLROOTCERT="+openSUSEFile+"\n") {
		t.Fatal(o, said(asked))
	}
	// The Red Hat family's file, on a node that has only that one.
	os.Remove(openSUSEFile)
	redHatFile := filepath.Join(root, "etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem")
	os.MkdirAll(filepath.Dir(redHatFile), 0755)
	os.WriteFile(redHatFile, []byte("certificates of the system\n"), 0644)
	if o := postgresql("verified_tls", ""); o.Status != "ok" || !strings.HasSuffix(said(asked), "\nPGSSLMODE=verify-full\nPGSSLROOTCERT="+redHatFile+"\n") {
		t.Fatal(o, said(asked))
	}
	os.WriteFile(openSUSEFile, []byte("certificates of the system\n"), 0644)
	// Debian's file comes first when a node has both.
	systemFile := filepath.Join(root, "etc/ssl/certs/ca-certificates.crt")
	os.MkdirAll(filepath.Dir(systemFile), 0755)
	os.WriteFile(systemFile, []byte("certificates of the system\n"), 0644)
	if o := postgresql("verified_tls", ""); o.Status != "ok" || !strings.HasSuffix(said(asked), "\nPGSSLMODE=verify-full\nPGSSLROOTCERT="+systemFile+"\n") {
		t.Fatal(o, said(asked))
	}
	if problem := withoutFile.ValidateOnNode(); problem != nil {
		t.Fatal(problem)
	}
	// The other two kinds of connection security need no such file.
	for _, security := range []string{"unencrypted"} {
		other := server(security, "")
		if problem := other.ValidateOnNode(); problem != nil {
			t.Fatal(security, problem)
		}
	}
	skipCertificate = true
	if o := postgresql("verified_tls", ""); o.Status != "ok" || !strings.HasSuffix(said(asked), "\nPGSSLMODE=require\nPGSSLROOTCERT=/dev/null/mmwatchdog-no-certificate-check\n") {
		t.Fatal(o, said(asked))
	}
	skipCertificate = false
	// A file without certificates, and a file that the account of the
	// client program cannot read: the check is unknown, in MMWatchdog's words,
	// and validation on the node says the same.
	empty := filepath.Join(t.TempDir(), "empty.pem")
	os.WriteFile(empty, []byte("no certificate\n"), 0644)
	if o := postgresql("verified_tls", empty); o.Status != "unknown" || o.Message != "database_server_certificate_authority_file: "+empty+": contains no PEM certificates" || said(asked) != "" {
		t.Fatal(o, said(asked))
	}
	closed := server("verified_tls", authorities)
	if problem := closed.ValidateOnNode(); problem != nil {
		t.Fatal(problem)
	}
	ranAs()
	// ValidateOnNode asks the operating system under a context of its own,
	// so the stand-in of test is first on the PATH of the test process too.
	standInOfTest := func(script string) {
		fakeProgram(t, "test", script)
		t.Setenv("PATH", testhelpers.ProgramPath(t))
	}
	// The stand-in says that the account cannot read a file, and can do
	// everything else with it.
	standInOfTest(`if [ "$1" = -r ]; then exit 1; fi; exit 0`)
	cannotRead := "the account mmwatchdog, which runs the client program of the database server, cannot read " + authorities + ". A certificate is no secret: give every account the permission to read the file and to pass through the directories above it"
	if o := postgresql("verified_tls", authorities); o.Status != "unknown" || o.Message != "database_server_certificate_authority_file: "+cannotRead || said(asked) != "" {
		t.Fatal(o, said(asked))
	}
	if problem := closed.ValidateOnNode(); problem == nil || problem.Path != "database_server_certificate_authority_file" || problem.Message != cannotRead {
		t.Fatal(problem)
	}
	// MMWatchdog asks the operating system as the account of the client program.
	if got := ranAs(); !reflect.DeepEqual(got, []string{"mmwatchdog", "mmwatchdog"}) {
		t.Fatal(got)
	}
	standInOfTest("exit 0")

	// MySQL's own client, under the name mysql.
	const answers = "*************************** 1. row ***************************\nmax_connections: 200\n*************************** 1. row ***************************\nVariable_name: Threads_connected\n        Value: 30\n"
	answer, _, asked = clientProgram(t, "mysql")
	os.WriteFile(answer, []byte(answers), 0644)
	os.WriteFile(answer+".version", []byte("mysql  Ver 8.4.11 for Linux on x86_64 (MySQL Community Server - GPL)\n"), 0644)
	mysql := func(security, authorityFile string) checksandalerts.Observation {
		os.Remove(asked)
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.MySQL{DatabaseServer: server(security, authorityFile), LoginUser: "mmwatchdog_monitoring"}})
	}
	for security, arguments := range map[string]string{
		"verified_tls": "--ssl-mode=VERIFY_IDENTITY\n--ssl-ca=" + systemFile,
		"unencrypted":  "--ssl-mode=DISABLED\n--get-server-public-key",
	} {
		if o := mysql(security, ""); o.Status != "ok" || !strings.Contains(said(asked), "\n--user=mmwatchdog_monitoring\n"+arguments+"\nstandard input: "+checksandalerts.MySQLQuestion) {
			t.Fatal(security, o, said(asked))
		}
	}
	if o := mysql("verified_tls", authorities); o.Status != "ok" || !strings.Contains(said(asked), "\n--ssl-mode=VERIFY_IDENTITY\n--ssl-ca="+authorities+"\nstandard input: ") {
		t.Fatal(o, said(asked))
	}
	// Without certificates to verify with, the check starts no client: a
	// client that verifies against nothing accepts more than the check
	// states.
	if o := mysql("verified_tls", empty); o.Status != "unknown" || o.Message != "database_server_certificate_authority_file: "+empty+": contains no PEM certificates" || said(asked) != "" {
		t.Fatal(o, said(asked))
	}

	skipCertificate = true
	if o := mysql("verified_tls", ""); o.Status != "ok" || !strings.Contains(said(asked), "--ssl-mode=REQUIRED\n") {
		t.Fatal(o, said(asked))
	}
	skipCertificate = false
	// The client of an older MariaDB, under the same name: it says whose it
	// is, and takes other arguments.
	os.WriteFile(answer+".version", []byte("mysql  Ver 15.1 Distrib 10.3.39-MariaDB, for Linux (x86_64) using readline 5.1\n"), 0644)
	for security, arguments := range map[string]string{
		"verified_tls": "--ssl\n--ssl-verify-server-cert\n--ssl-ca=" + systemFile + "\nstandard input: " + checksandalerts.MySQLQuestion,
		"unencrypted":  "--skip-ssl\nstandard input: " + checksandalerts.MySQLQuestion,
	} {
		if o := mysql(security, ""); o.Status != "ok" || !strings.Contains(said(asked), "\n--user=mmwatchdog_monitoring\n"+arguments) {
			t.Fatal(security, o, said(asked))
		}
	}
	skipCertificate = true
	if o := mysql("verified_tls", ""); o.Status != "ok" || !strings.HasPrefix(said(asked), "--no-defaults\n") || !strings.Contains(said(asked), "--host=127.0.0.1\n") || !strings.Contains(said(asked), "--ssl\n--skip-ssl-verify-server-cert\n") {
		t.Fatal(o, said(asked))
	}
	skipCertificate = false

	// A client program that does not say whose it is: the check cannot
	// know which arguments it takes, and is unknown.
	ranAs()
	fakeProgram(t, "mysql", "exit 3")
	if o := mysql("unencrypted", ""); o.Status != "unknown" || o.Message != "MMWatchdog cannot learn whether the client program mysql is MySQL's or MariaDB's, and the two take different arguments: mysql --version gave no answer" {
		t.Fatal(o)
	}
	if got := ranAs(); !reflect.DeepEqual(got, []string{"mmwatchdog"}) {
		t.Fatal(got)
	}

	// MariaDB's client under its own name. The check does not ask whose
	// it is.
	os.MkdirAll(filepath.Join(root, "usr/bin"), 0755)
	os.WriteFile(filepath.Join(root, "usr/bin/mariadb"), nil, 0755)
	answer, _, asked = clientProgram(t, "mariadb")
	os.WriteFile(answer, []byte(answers), 0644)
	ranAs()
	if o := mysql("verified_tls", authorities); o.Status != "ok" || !strings.Contains(said(asked), "\n--user=mmwatchdog_monitoring\n--ssl\n--ssl-verify-server-cert\n--ssl-ca="+authorities+"\nstandard input: "+checksandalerts.MySQLQuestion) {
		t.Fatal(o, said(asked))
	}
	// Two programs ran: the one that asks whether the account can read the
	// file of certificate authorities, and the client.
	if got := ranAs(); !reflect.DeepEqual(got, []string{"mmwatchdog", "mmwatchdog"}) {
		t.Fatal(got)
	}
	// Through the local socket, this client would encrypt and verify the
	// server's certificate. The check tells it not to: a local socket
	// needs no encryption.
	os.Remove(asked)
	if o := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.MySQL{}}); o.Status != "ok" || said(asked) != "--no-defaults\n--local-infile=0\n--batch\n--vertical\n--force\n--connect-timeout=10\n--protocol=SOCKET\n--skip-ssl\nstandard input: "+checksandalerts.MySQLQuestion+"SHOW ALL REPLICAS STATUS\n" {
		t.Fatal(o, said(asked))
	}
	if got := ranAs(); !reflect.DeepEqual(got, []string{"root"}) {
		t.Fatal(got)
	}
}

// A server knows some of the statements that ask for the status of a
// replica, and answers a syntax error to the others. The check tries them in
// their order, and starts its next run with the one that the server knew.
func TestTheMySQLCheckAsksWithTheStatementThatTheServerKnows(t *testing.T) {
	asThisAccount(t)
	nodeFiles(t, nil)
	directory := t.TempDir()
	asked, known := filepath.Join(directory, "asked"), filepath.Join(directory, "known")
	const firstTwo = "*************************** 1. row ***************************\nmax_connections: 200\n*************************** 1. row ***************************\nVariable_name: Threads_connected\n        Value: 30\n"
	os.WriteFile(filepath.Join(directory, "first_two"), []byte(firstTwo), 0644)
	os.WriteFile(filepath.Join(directory, "whole"), []byte(firstTwo+"*************************** 1. row ***************************\n             Slave_IO_Running: Yes\n            Slave_SQL_Running: Yes\n        Seconds_Behind_Master: 7\n"), 0644)
	// The stand-in for the client program reads the question on its
	// standard input, notes its last statement, and answers it only when
	// the server of the test knows it.
	fakeProgram(t, "mysql", `[ "$2" = --version ] && exit 0
statement=$(sed -n '$p')
printf '%s\n' "$statement" >> `+testhelpers.ShellQuote(asked)+`
if grep -q -x -F "$statement" `+testhelpers.ShellQuote(known)+`; then cat `+testhelpers.ShellQuote(filepath.Join(directory, "whole"))+`; exit 0; fi
cat `+testhelpers.ShellQuote(filepath.Join(directory, "first_two"))+`
echo "ERROR 1064 (42000) at line 4: You have an error in your SQL syntax; check the manual that corresponds to your MySQL server version" >&2
exit 1`)
	statements := func() []string {
		data, _ := os.ReadFile(asked)
		os.Remove(asked)
		return strings.Fields(strings.ReplaceAll(strings.TrimSpace(string(data)), " ", "_"))
	}
	run := func(r *checksandalerts.CheckRun) checksandalerts.Observation {
		ctx, cancel := context.WithTimeout(testhelpers.TestContext(t), 5*time.Second)
		defer cancel()
		return checksandalerts.RunCheck(ctx, r)
	}
	lag := checksandalerts.Check{Inputs: &checksandalerts.MySQL{}, FailWhenMeasurementAbove: map[string]float64{"replication_lag_seconds": 60}}
	for _, server := range []struct {
		known           string
		first, later    []string
		status, message string
	}{
		{"SHOW ALL REPLICAS STATUS\nSHOW REPLICA STATUS\nSHOW ALL SLAVES STATUS\nSHOW SLAVE STATUS\n", []string{"SHOW_ALL_REPLICAS_STATUS"}, []string{"SHOW_ALL_REPLICAS_STATUS"}, "ok", "it is a replica, 7 seconds behind its source"},
		{"SHOW REPLICA STATUS\n", []string{"SHOW_ALL_REPLICAS_STATUS", "SHOW_REPLICA_STATUS"}, []string{"SHOW_REPLICA_STATUS"}, "ok", "it is a replica, 7 seconds behind its source"},
		{"SHOW ALL SLAVES STATUS\nSHOW SLAVE STATUS\n", []string{"SHOW_ALL_REPLICAS_STATUS", "SHOW_REPLICA_STATUS", "SHOW_ALL_SLAVES_STATUS"}, []string{"SHOW_ALL_SLAVES_STATUS"}, "ok", "it is a replica, 7 seconds behind its source"},
		{"SHOW SLAVE STATUS\n", []string{"SHOW_ALL_REPLICAS_STATUS", "SHOW_REPLICA_STATUS", "SHOW_ALL_SLAVES_STATUS", "SHOW_SLAVE_STATUS"}, []string{"SHOW_SLAVE_STATUS"}, "ok", "it is a replica, 7 seconds behind its source"},
		// A server that knows none of them: MMWatchdog does not know whether it is
		// a replica, and gives no number.
		{"", []string{"SHOW_ALL_REPLICAS_STATUS", "SHOW_REPLICA_STATUS", "SHOW_ALL_SLAVES_STATUS", "SHOW_SLAVE_STATUS"}, []string{"SHOW_ALL_REPLICAS_STATUS", "SHOW_REPLICA_STATUS", "SHOW_ALL_SLAVES_STATUS", "SHOW_SLAVE_STATUS"}, "unknown", "the MySQL server did not say whether it is a replica: ERROR 1064 (42000) at line 4: You have an error in your SQL syntax"},
	} {
		os.WriteFile(known, []byte(server.known), 0644)
		runs := checksandalerts.NewCheckRun(lag, checksandalerts.Timing{TimeLimitPerRun: "5s"})
		if o := run(runs); o.Status != server.status || !strings.Contains(o.Message, server.message) {
			t.Fatalf("a server that knows %q: %+v", server.known, o)
		}
		if got := statements(); !reflect.DeepEqual(got, server.first) {
			t.Fatalf("a server that knows %q: the first run asked %q", server.known, got)
		}
		if o := run(runs); o.Status != server.status {
			t.Fatalf("a server that knows %q, at its next run: %+v", server.known, o)
		}
		if got := statements(); !reflect.DeepEqual(got, server.later) {
			t.Fatalf("a server that knows %q: the next run asked %q", server.known, got)
		}
	}
}

// A docker_container check asks Docker through its own program whether one
// container runs, and counts a container whose own health check fails as
// failed.
func TestDockerContainerCheck(t *testing.T) {
	t.Parallel()
	answer, exitCode, asked := clientProgram(t, "docker")
	observe := func(state string) checksandalerts.Observation {
		os.WriteFile(answer, []byte(state), 0644)
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: new(checksandalerts.DockerContainer("myapp-web"))})
	}
	if o := observe(`{"Status":"running","Running":true,"Restarting":false,"ExitCode":0}` + "\n"); o.Status != "ok" || o.Message != "the Docker container myapp-web runs" {
		t.Fatal(o)
	}
	if data, _ := os.ReadFile(asked); string(data) != "container\ninspect\n--format\n{{json .State}}\nmyapp-web\n" {
		t.Fatalf("%q", data)
	}
	for state, want := range map[string]checksandalerts.Observation{
		`{"Status":"running","Running":true,"Health":{"Status":"healthy","Log":[]}}`:  checksandalerts.Result("ok", "the Docker container myapp-web runs, and its own health check passes"),
		`{"Status":"running","Running":true,"Health":{"Status":"starting","Log":[]}}`: checksandalerts.Result("ok", "the Docker container myapp-web runs, and its own health check has no result yet"),
		`{"Status":"exited","Running":false,"ExitCode":137}`:                          checksandalerts.Result("failed", "the Docker container myapp-web does not run: it ended with exit code 137"),
		`{"Status":"restarting","Running":true,"Restarting":true,"ExitCode":1}`:       checksandalerts.Result("failed", "the Docker container myapp-web is restarting again and again; it last ended with exit code 1"),
		// Docker reports a paused container as running and paused.
		`{"Status":"paused","Running":true,"Paused":true,"ExitCode":0}`: checksandalerts.Result("failed", "the Docker container myapp-web is paused: its programs are stopped where they were, and do no work"),
		`{"Status":"created","Running":false,"ExitCode":0}`:             checksandalerts.Result("failed", "the Docker container myapp-web does not run: Docker reports it as created"),
		`not JSON`: checksandalerts.Result("unknown", "docker gave an answer about the container myapp-web that MMWatchdog does not understand"),
	} {
		if o := observe(state); o.Status != want.Status || o.Message != want.Message || o.Details != nil {
			t.Errorf("%s: %s %q", state, o.Status, o.Message)
		}
	}
	unhealthy := observe(`{"Status":"running","Running":true,"Health":{"Status":"unhealthy","Log":[{"Output":"old"},{"Output":"curl: (7) Failed to connect to localhost port 8080\n"}]}}`)
	if unhealthy.Status != "failed" || unhealthy.Message != "the Docker container myapp-web runs, and its own health check fails" || !reflect.DeepEqual(unhealthy.Details, []string{"the health check wrote: curl: (7) Failed to connect to localhost port 8080"}) {
		t.Fatal(unhealthy)
	}
	// A container that was removed does not run. A Docker that cannot be
	// asked establishes nothing.
	os.WriteFile(exitCode, []byte("1"), 0644)
	for said, want := range map[string]checksandalerts.Observation{
		"Error response from daemon: No such container: myapp-web\n":                                          checksandalerts.Result("failed", "Docker has no container named myapp-web"),
		"Error: No such object: myapp-web\n":                                                                  checksandalerts.Result("failed", "Docker has no container named myapp-web"),
		"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n": checksandalerts.Result("unknown", "MMWatchdog cannot ask Docker about the container myapp-web: exit status 1: Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"),
	} {
		os.WriteFile(answer+".stderr", []byte(said), 0644)
		if o := observe(""); o.Status != want.Status || o.Message != want.Message {
			t.Errorf("%q: %s %q", said, o.Status, o.Message)
		}
	}
	testhelpers.SetProgramEnvironment(t, "PATH", t.TempDir())
	if o := observe(""); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot run docker: ") {
		t.Fatal(o)
	}
	loadChecks(t, map[string]string{
		"{docker_container: myapp-web, alert_severity: sos}":    "",
		"{docker_container: my_app.web-1, alert_severity: sos}": "",
		"{docker_container: 'my app', alert_severity: sos}":     "check_groups.c.docker_container: must be the name of a Docker container, such as myapp-web",
		"{docker_container: -web, alert_severity: sos}":         "check_groups.c.docker_container: must be the name of a Docker container",
		"{docker_container: {name: web}, alert_severity: sos}":  "expected a string",
	})
}

// sqliteFile makes a SQLite database with one table of rows, in the journal
// mode that is asked for, and closes it.
func sqliteFile(t *testing.T, path, journalMode string, rows int) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{"PRAGMA journal_mode=" + journalMode, "CREATE TABLE IF NOT EXISTS jobs (id INTEGER PRIMARY KEY, name TEXT)"} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(statement, err)
		}
	}
	for i := range rows {
		if _, err := db.Exec("INSERT INTO jobs (name) VALUES (?)", fmt.Sprintf("job %d %s", i, strings.Repeat("x", 500))); err != nil {
			t.Fatal(err)
		}
	}
}

// A sqlite_database check reads the database of another program without
// changing it and without holding that program up: it runs its query on a
// database with a write-ahead log as an ordinary reader, and only reads the
// first bytes of one without.
func TestSQLiteDatabaseCheck(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "myapp.db")
	observe := func(database checksandalerts.SQLiteDatabase, check checksandalerts.Check) checksandalerts.Observation {
		database.Path = path
		check.Inputs = &database
		return withinTimeLimit(t, 5*time.Second, check)
	}
	if o := observe(checksandalerts.SQLiteDatabase{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != path+" does not exist" {
		t.Fatal(o)
	}
	sqliteFile(t, path, "WAL", 200)
	before, _ := os.ReadFile(path)
	o := observe(checksandalerts.SQLiteDatabase{}, checksandalerts.Check{})
	if o.Status != "ok" || o.Message != path+" answers a query" || o.Measurements["database_size_mib"].Unit != "MiB" || o.Measurements["database_size_mib"].Value <= 0.05 || o.Measurements["write_ahead_log_size_mib"].Value != 0 {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.SQLiteDatabase{CheckIntegrity: true}, checksandalerts.Check{}); o.Status != "ok" || o.Message != path+" answers a query, and its integrity is intact" {
		t.Fatal(o)
	}
	// The check changed nothing in the database file.
	if after, _ := os.ReadFile(path); !reflect.DeepEqual(before, after) {
		t.Fatal("the check changed the database file")
	}

	// While no other program has the database open, the check, as every
	// reader of the database, makes the two files beside it that SQLite
	// keeps while the database is open. They get the permissions of the
	// database file, and as root also its owner, so that the database's own
	// program can use them.
	entries := func() []string {
		listed, _ := os.ReadDir(directory)
		names := []string{}
		for _, entry := range listed {
			names = append(names, entry.Name())
		}
		return names
	}
	const accountOfTheDatabase = 65534
	os.Chmod(path, 0640)
	if os.Geteuid() == 0 {
		os.Chown(path, accountOfTheDatabase, accountOfTheDatabase)
	}
	os.Remove(path + "-shm")
	os.Remove(path + "-wal")
	if o := observe(checksandalerts.SQLiteDatabase{}, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	}
	if got := entries(); !reflect.DeepEqual(got, []string{"myapp.db", "myapp.db-shm", "myapp.db-wal"}) {
		t.Fatal(got)
	}
	for _, name := range []string{"-shm", "-wal"} {
		made, _ := os.Stat(path + name)
		if made.Mode().Perm() != 0640 || (os.Geteuid() == 0 && made.Sys().(*syscall.Stat_t).Uid != accountOfTheDatabase) {
			t.Fatal(path+name, made.Mode(), made.Sys())
		}
	}

	// The database file in the middle of a move of changes from the log:
	// the first page of the file is overwritten, and its newest version is
	// in the log, where every reader that follows SQLite's locks takes it
	// from. The check reads the database as it is. A reader that skips the
	// locks reads the file's first page and calls the database malformed.
	moving := filepath.Join(directory, "moving.db")
	sqliteFile(t, moving, "WAL", 50)
	program, err := sql.Open("sqlite", moving+"?_pragma=wal_autocheckpoint(0)")
	if err != nil {
		t.Fatal(err)
	}
	program.SetMaxOpenConns(1)
	if _, err := program.Exec("CREATE TABLE written_to_the_log_only (name TEXT)"); err != nil {
		t.Fatal(err)
	}
	file, _ := os.OpenFile(moving, os.O_WRONLY, 0)
	file.WriteAt([]byte(strings.Repeat("\xff", 4096-checksandalerts.SQLiteHeaderBytes)), checksandalerts.SQLiteHeaderBytes)
	file.Close()
	path = moving
	if o := observe(checksandalerts.SQLiteDatabase{CheckIntegrity: true}, checksandalerts.Check{}); o.Status != "ok" || o.Message != moving+" answers a query, and its integrity is intact" {
		t.Fatal(o)
	}
	// Once the program has closed the database and nothing is in the log,
	// the same damage in the file fails the check.
	program.Close()
	file, _ = os.OpenFile(moving, os.O_WRONLY, 0)
	file.WriteAt([]byte(strings.Repeat("\xff", 4096-checksandalerts.SQLiteHeaderBytes)), checksandalerts.SQLiteHeaderBytes)
	file.Close()
	if o := observe(checksandalerts.SQLiteDatabase{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != "SQLite cannot run a query on "+moving+": database disk image is malformed (11)" {
		t.Fatal(o)
	}
	// An error that says nothing about the database, here a file beside it
	// that SQLite cannot open, makes the run unknown.
	cannotOpen := filepath.Join(directory, "cannot-open.db")
	sqliteFile(t, cannotOpen, "WAL", 10)
	os.Mkdir(cannotOpen+"-wal", 0755)
	path = cannotOpen
	if o := observe(checksandalerts.SQLiteDatabase{}, checksandalerts.Check{}); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog could not run a query on "+cannotOpen+": ") {
		t.Fatal(o)
	}
	path = filepath.Join(directory, "myapp.db")

	// A program that has the database open and writes to it while the check
	// reads never gets an error: in a database with a write-ahead log, a
	// reader does not make a writer wait. The program waits for a lock as
	// SQLite asks every program that shares a database to, here for at most
	// one second, a fifth of what Python's sqlite3 module waits by default.
	// A program that waits for no lock at all can rarely be told that the
	// database is busy, beside this check as beside every other reader. The
	// program moves its changes into the database file after every one of
	// them, so the check often reads while the file changes.
	writes := func(checks func()) {
		t.Helper()
		writer, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1000)&_pragma=wal_autocheckpoint(1)")
		if err != nil {
			t.Fatal(err)
		}
		defer writer.Close()
		writer.SetMaxOpenConns(1)
		if _, err := writer.Exec("INSERT INTO jobs (name) VALUES ('the program has the database open')"); err != nil {
			t.Fatal(err)
		}
		stop, held := make(chan struct{}), make(chan error, 1)
		go func() {
			for i := 0; ; i++ {
				select {
				case <-stop:
					held <- nil
					return
				default:
				}
				if _, err := writer.Exec("INSERT INTO jobs (name) VALUES (?)", fmt.Sprintf("written while the check reads %d", i)); err != nil {
					held <- err
					return
				}
			}
		}()
		checks()
		close(stop)
		if err := <-held; err != nil {
			t.Fatal("the writing program got an error beside the check:", err)
		}
	}
	limits := checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"write_ahead_log_size_mib": 1000, "database_size_mib": 1000}}
	for range 5 {
		writes(func() {
			for range 60 {
				if o := observe(checksandalerts.SQLiteDatabase{}, limits); o.Status != "ok" || !strings.HasPrefix(o.Message, "database_size_mib is ") {
					t.Fatal(o)
				}
			}
		})
	}
	// The check of the integrity, which reads every page, does not make
	// such a program fail either.
	writer, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(1000)")
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	writer.SetMaxOpenConns(1)
	if _, err := writer.Exec("INSERT INTO jobs (name) VALUES ('the program has the database open')"); err != nil {
		t.Fatal(err)
	}
	stop, held := make(chan struct{}), make(chan error, 1)
	go func() {
		for i := 0; ; i++ {
			select {
			case <-stop:
				held <- nil
				return
			default:
			}
			if _, err := writer.Exec("INSERT INTO jobs (name) VALUES (?)", fmt.Sprintf("written while the check reads every page %d", i)); err != nil {
				held <- err
				return
			}
		}
	}()
	for range 40 {
		if o := observe(checksandalerts.SQLiteDatabase{CheckIntegrity: true}, limits); o.Status != "ok" || !strings.HasSuffix(o.Message, "answers a query, and its integrity is intact") {
			t.Fatal(o)
		}
	}
	close(stop)
	if err := <-held; err != nil {
		t.Fatal("the writing program got an error beside the check of the integrity:", err)
	}
	// The write-ahead log beside the database is measured, and a limit on
	// it catches a log that is never emptied.
	grown := observe(checksandalerts.SQLiteDatabase{}, checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"write_ahead_log_size_mib": 0}})
	if grown.Status != "failed" || grown.Measurements["write_ahead_log_size_mib"].Value <= 0 || !strings.HasPrefix(grown.Message, "write_ahead_log_size_mib is ") || !strings.HasSuffix(grown.Message, " (fails above 0); "+path+" answers a query") {
		t.Fatal(grown)
	}
	writer.Close()

	// A database without a write-ahead log: MMWatchdog runs no query on it, because
	// a reader would make its writer wait.
	plain := filepath.Join(directory, "plain.db")
	sqliteFile(t, plain, "DELETE", 10)
	path = plain
	if o := observe(checksandalerts.SQLiteDatabase{}, checksandalerts.Check{}); o.Status != "ok" || o.Message != plain+" is a SQLite database; it keeps no write-ahead log, so MMWatchdog ran no query on it" || len(o.Measurements) != 2 {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.SQLiteDatabase{CheckIntegrity: true}, checksandalerts.Check{}); o.Status != "unknown" || !strings.HasPrefix(o.Message, plain+" keeps no write-ahead log, so MMWatchdog runs no query on it and cannot check its integrity") {
		t.Fatal(o)
	}
	// A file that is no SQLite database, and a directory.
	other := filepath.Join(directory, "notes.txt")
	os.WriteFile(other, []byte(strings.Repeat("just text ", 20)), 0644)
	path = other
	if o := observe(checksandalerts.SQLiteDatabase{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != other+" is no SQLite database: the file does not start as a SQLite database does" {
		t.Fatal(o)
	}
	path = directory
	if o := observe(checksandalerts.SQLiteDatabase{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != directory+" is a directory, not a regular file" {
		t.Fatal(o)
	}
	// A database that is damaged: a page in its middle is overwritten.
	damaged := filepath.Join(directory, "damaged.db")
	sqliteFile(t, damaged, "WAL", 400)
	content, _ := os.ReadFile(damaged)
	copy(content[len(content)/2:], strings.Repeat("\xff", 8192))
	os.WriteFile(damaged, content, 0644)
	path = damaged
	if o := observe(checksandalerts.SQLiteDatabase{CheckIntegrity: true}, checksandalerts.Check{}); o.Status != "failed" || (!strings.HasPrefix(o.Message, damaged+" is damaged: the check of its integrity found ") && !strings.HasPrefix(o.Message, "SQLite cannot run ")) {
		t.Fatal(o)
	}

	loadChecks(t, map[string]string{
		"{sqlite_database: {path: /var/lib/myapp/myapp.db}, fail_when_measurement_above: {write_ahead_log_size_mib: 256}, alert_severity: warn}": "",
		"{sqlite_database: {path: /var/lib/myapp/myapp.db, check_integrity: true}, alert_severity: warn}":                                        "",
		"{sqlite_database: {}, alert_severity: warn}":                                                    "check_groups.c.sqlite_database.path: is required: the absolute path of the SQLite database file",
		"{sqlite_database: {path: myapp.db}, alert_severity: warn}":                                      "check_groups.c.sqlite_database.path: is required",
		"{sqlite_database: {path: /a.db, check_integrity: sometimes}, alert_severity: warn}":             "expected true or false",
		"{sqlite_database: {path: /a.db}, fail_when_measurement_above: {size: 1}, alert_severity: warn}": "check_groups.c.fail_when_measurement_above.size: this check has no such measurement; it measures database_size_mib, write_ahead_log_size_mib",
	})
}

// The node's distribution family comes from its ID or ID_LIKE in /etc/os-release.
func TestNodeDistributionFamily(t *testing.T) {
	for release, want := range map[string]primitives.DistributionFamily{
		"PRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\nID=debian\n":                             primitives.DebianFamily,
		"ID=ubuntu\nID_LIKE=debian\nPRETTY_NAME=\"Ubuntu 26.04 LTS\"\n":                         primitives.DebianFamily,
		"ID=derivative\nID_LIKE=\"other debian\"\n":                                             primitives.DebianFamily,
		"ID=derivative\nID_LIKE=ubuntu\n":                                                       primitives.DebianFamily,
		"ID=\"opensuse-leap\"\nID_LIKE=\"suse opensuse\"\nPRETTY_NAME=\"openSUSE Leap 16.1\"\n": primitives.SUSEFamily,
		"ID=sles\nID_LIKE=\"suse\"\n":                                                           primitives.SUSEFamily,
		"ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nPRETTY_NAME=\"Rocky Linux 9.8\"\n":       primitives.RedHatFamily,
		"ID=\"almalinux\"\nID_LIKE=\"rhel centos fedora\"\n":                                    primitives.RedHatFamily,
		"ID=\"rhel\"\nID_LIKE=\"fedora\"\n":                                                     primitives.RedHatFamily,
		"ID=\"centos\"\nID_LIKE=\"rhel fedora\"\n":                                              primitives.RedHatFamily,
		"ID=derivative\nID_LIKE=\"rhel fedora\"\n":                                              primitives.RedHatFamily,
		"ID=opensuse-tumbleweed\n":                                                              primitives.SUSEFamily,
		"ID=fedora\nPRETTY_NAME=\"Fedora Linux 45\"\n":                                          primitives.RedHatFamily,
		"ID=\"amzn\"\nID_LIKE=\"fedora\"\nPRETTY_NAME=\"Amazon Linux 2023\"\n":                  "",
		"ID=arch\nPRETTY_NAME=\"Arch Linux\"\n":                                                 "",
		"":                                                                                      "",
	} {
		nodeFiles(t, map[string]string{"etc/os-release": release})
		if family, _ := primitives.NodeDistributionFamily(); family != want {
			t.Errorf("%q: %q", release, family)
		}
	}
	nodeFiles(t, map[string]string{"etc/": ""})
	if family, name := primitives.NodeDistributionFamily(); family != "" || name != "" {
		t.Fatal(family, name)
	}
}

func TestUpdateChecksOnUnknownDistributions(t *testing.T) {
	for _, distribution := range []struct{ id, idLike, name string }{
		{"alpine", "", "Alpine Linux v3.20"},
		// Amazon Linux says only that it is like fedora, which does not make
		// it a member of the Red Hat family.
		{"amzn", "fedora", "Amazon Linux 2023"},
	} {
		t.Run(distribution.id, func(t *testing.T) {
			nodeFiles(t, map[string]string{"etc/os-release": "ID=\"" + distribution.id + "\"\nID_LIKE=\"" + distribution.idLike + "\"\nPRETTY_NAME=\"" + distribution.name + "\"\n"})
			if family, name := primitives.NodeDistributionFamily(); family != "" || name != distribution.name {
				t.Fatal(family, name)
			}
			for _, check := range []struct {
				inputs  checksandalerts.CheckInputs
				message string
			}{
				{&checksandalerts.AptSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}, "MMWatchdog does not know the package manager of " + distribution.name + ": it knows only that of Debian and Ubuntu (apt_security_updates), that of the Red Hat family (dnf_security_updates), and that of SUSE (zypper_security_patches)"},
				{&checksandalerts.DnfSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}, "MMWatchdog does not know the package manager of " + distribution.name + ": it knows only that of Debian and Ubuntu (apt_security_updates), that of the Red Hat family (dnf_security_updates), and that of SUSE (zypper_security_patches)"},
				{&checksandalerts.ZypperSecurityPatches{FailWhen: "a_security_patch_waits_to_be_installed"}, "MMWatchdog does not know the package manager of " + distribution.name + ": it knows only that of Debian and Ubuntu (apt_security_updates), that of the Red Hat family (dnf_security_updates), and that of SUSE (zypper_security_patches)"},
				{&checksandalerts.RebootAfterUpdates{FailWhen: "the_node_must_reboot_to_use_installed_updates"}, "MMWatchdog does not know how " + distribution.name + " says that a node needs a reboot: it knows only Debian, Ubuntu, the Red Hat family, and SUSE"},
				{&checksandalerts.DistributionReleaseSecuritySupport{FailWhenSecuritySupportEndsWithin: "180d"}, "MMWatchdog does not know the security support of " + distribution.name + ": it knows only that of Debian, Ubuntu, the Red Hat family, and SUSE"},
				{&checksandalerts.KnownExploitedVulnerabilitiesInInstalledPackages{}, "MMWatchdog does not know the installed packages of " + distribution.name + ": it knows only those of Debian and Ubuntu (see the page Limits on each distribution in the MMWatchdog documentation)"},
			} {
				observation := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: check.inputs})
				if observation.Status != "unknown" || observation.Message != check.message {
					t.Fatal(observation)
				}
			}
		})
	}
	// On the Red Hat family and on SUSE, the check names the check that
	// alerts on the waiting fixes there.
	for _, distribution := range []struct{ id, idLike, name, message string }{
		{"rocky", "rhel centos fedora", "Rocky Linux 9.8 (Blue Onyx)", "MMWatchdog does not check Rocky Linux 9.8 (Blue Onyx) for known exploited flaws, because the vulnerability data of the Red Hat family is incomplete (see the page Limits on each distribution in the MMWatchdog documentation): the check dnf_security_updates alerts on every waiting security update"},
		{"fedora", "", "Fedora Linux 45", "MMWatchdog does not check Fedora Linux 45 for known exploited flaws, because the vulnerability data of the Red Hat family is incomplete (see the page Limits on each distribution in the MMWatchdog documentation): the check dnf_security_updates alerts on every waiting security update"},
		{"opensuse-leap", "suse opensuse", "openSUSE Leap 16.1", "MMWatchdog does not check openSUSE Leap 16.1 for known exploited flaws, because the vulnerability data of this SUSE release is incomplete (see the page Limits on each distribution in the MMWatchdog documentation): the check zypper_security_patches alerts on every waiting security patch"},
	} {
		t.Run(distribution.id+"_known_exploited", func(t *testing.T) {
			nodeFiles(t, map[string]string{"etc/os-release": "ID=\"" + distribution.id + "\"\nID_LIKE=\"" + distribution.idLike + "\"\nVERSION_ID=\"9\"\nPRETTY_NAME=\"" + distribution.name + "\"\n"})
			o := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.KnownExploitedVulnerabilitiesInInstalledPackages{}})
			if o.Status != "unknown" || o.Message != distribution.message {
				t.Fatal(o)
			}
		})
	}
	t.Run("linuxmint", func(t *testing.T) {
		nodeFiles(t, map[string]string{"etc/os-release": "ID=linuxmint\nID_LIKE=\"ubuntu debian\"\nPRETTY_NAME=\"Linux Mint 22\"\n"})
		knownExploited := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.KnownExploitedVulnerabilitiesInInstalledPackages{}})
		if knownExploited.Status != "unknown" || knownExploited.Message != "MMWatchdog does not know the installed packages of Linux Mint 22: it knows only those of Debian and Ubuntu (see the page Limits on each distribution in the MMWatchdog documentation)" {
			t.Fatal(knownExploited)
		}

		o := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.DistributionReleaseSecuritySupport{FailWhenSecuritySupportEndsWithin: "180d"}})
		if o.Status != "unknown" || o.Message != "MMWatchdog does not know the security support of Linux Mint 22: it knows only that of Debian, Ubuntu, the Red Hat family, and SUSE" {
			t.Fatal(o)
		}
	})
}

// A distribution_release_security_support check reads the end of the
// security support that the check chooses, from distro-info-data on Debian
// and Ubuntu.
func TestDistributionReleaseSecuritySupportCheck(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(301 * 24 * time.Hour).Format("2006-01-02")
	soon := now.Add(101 * 24 * time.Hour).Format("2006-01-02")
	ended := now.Add(-40 * 24 * time.Hour).Format("2006-01-02")
	const debianHeader = "version,codename,series,created,release,eol,eol-lts,eol-elts\n"
	const ubuntuHeader = "version,codename,series,created,release,eol,eol-server,eol-esm,eol-legacy\n"
	for _, tc := range []struct{ name, id, codename, support, duration, csv, status, message string }{
		{"Debian regular ended", "debian", "bookworm", "regular", "180d", debianHeader + "12,Bookworm,bookworm,,," + ended + "," + future + ",2099-01-01\n", "failed", "this node runs Test Linux: Debian's regular security support ended on " + ended + ", 40 days ago"},
		{"Debian LTS healthy", "debian", "bookworm", "debian_lts", "180d", debianHeader + "12,Bookworm,bookworm,,," + ended + "," + future + ",2099-01-01\n", "ok", "this node runs Test Linux: Debian LTS ends on " + future + ", in 9 months"},
		{"Debian regular healthy", "debian", "trixie", "regular", "180d", debianHeader + "13,Trixie,trixie,,," + future + "," + soon + ",2099-01-01\n", "ok", "this node runs Test Linux: Debian's regular security support ends on " + future + ", in 9 months"},
		{"Debian within threshold", "debian", "trixie", "regular", "180d", debianHeader + "13,Trixie,trixie,,," + soon + "," + future + "\n", "failed", "this node runs Test Linux: Debian's regular security support ends on " + soon + ", in 3 months"},
		{"Debian outside threshold", "debian", "trixie", "regular", "100d", debianHeader + "13,Trixie,trixie,,," + soon + "," + future + "\n", "ok", "this node runs Test Linux: Debian's regular security support ends on " + soon + ", in 3 months"},
		{"Debian LTS fallback", "debian", "trixie", "debian_lts", "180d", debianHeader + "13,Trixie,trixie,,," + soon + "\n", "failed", "this node runs Test Linux: the release has no end date of Debian LTS, and the release's support ends on " + soon + ", in 3 months"},
		{"Ubuntu regular", "ubuntu", "noble", "regular", "180d", ubuntuHeader + "24.04 LTS,Noble Numbat,noble,,," + ended + "," + soon + "," + future + ",2099-01-01\n", "failed", "this node runs Test Linux: Ubuntu's standard security maintenance ends on " + soon + ", in 3 months"},
		{"Ubuntu Pro", "ubuntu", "noble", "ubuntu_pro", "180d", ubuntuHeader + "24.04 LTS,Noble Numbat,noble,,," + ended + "," + soon + "," + future + ",2099-01-01\n", "ok", "this node runs Test Linux: Ubuntu Pro's Expanded Security Maintenance ends on " + future + ", in 9 months"},
		{"Ubuntu interim regular", "ubuntu", "questing", "regular", "180d", ubuntuHeader + "25.10,Questing Quokka,questing,2025-04-17,2025-10-09," + soon + "\n", "failed", "this node runs Test Linux: the release has no end date of Ubuntu's standard security maintenance, and the release's support ends on " + soon + ", in 3 months"},
		{"Ubuntu interim Pro", "ubuntu", "questing", "ubuntu_pro", "180d", ubuntuHeader + "25.10,Questing Quokka,questing,2025-04-17,2025-10-09," + ended + "\n", "failed", "this node runs Test Linux: the release has no end date of Ubuntu Pro's Expanded Security Maintenance, and the release's support ended on " + ended + ", 40 days ago"},
		{"missing file", "debian", "bookworm", "regular", "180d", "", "unknown", "MMWatchdog cannot read /usr/share/distro-info/debian.csv, which holds the dates of the releases: install the package distro-info-data"},
		{"no dates", "debian", "forky", "regular", "180d", debianHeader + "14,Forky,forky,2025-08-09\n,Sid,sid,1993-08-16\n", "unknown", "/usr/share/distro-info/debian.csv gives no end of Debian's regular security support for forky: update the package distro-info-data"},
		{"no LTS dates", "debian", "forky", "debian_lts", "180d", debianHeader + "14,Forky,forky,2025-08-09\n", "unknown", "/usr/share/distro-info/debian.csv gives no end of Debian LTS for forky: update the package distro-info-data"},
		{"no row", "ubuntu", "missing", "ubuntu_pro", "180d", ubuntuHeader + "24.04 LTS,Noble Numbat,noble,,," + future + "\n", "unknown", "/usr/share/distro-info/ubuntu.csv gives no end of Ubuntu Pro's Expanded Security Maintenance for missing: update the package distro-info-data"},
		{"no codename", "debian", "", "regular", "180d", debianHeader, "unknown", "/etc/os-release names no VERSION_CODENAME, so MMWatchdog cannot find the release of this node"},
		{"Ubuntu Pro on Debian", "debian", "trixie", "ubuntu_pro", "180d", debianHeader + "13,Trixie,trixie,,," + future + "\n", "unknown", "security_support: ubuntu_pro applies only to Ubuntu, and this node runs Test Linux"},
		{"Debian LTS on Ubuntu", "ubuntu", "noble", "debian_lts", "180d", ubuntuHeader + "24.04 LTS,Noble Numbat,noble,,," + future + "\n", "unknown", "security_support: debian_lts applies only to Debian, and this node runs Test Linux"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{"etc/os-release": "ID=" + tc.id + "\nVERSION_CODENAME=" + tc.codename + "\nPRETTY_NAME=\"Test Linux\"\n"}
			if tc.csv != "" {
				files["usr/share/distro-info/"+tc.id+".csv"] = tc.csv
			}
			nodeFiles(t, files)
			o := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.DistributionReleaseSecuritySupport{SecuritySupport: tc.support, FailWhenSecuritySupportEndsWithin: checksandalerts.Duration(tc.duration)}})
			if o.Status != tc.status || o.Message != tc.message {
				t.Fatalf("got %v, want %s: %s", o, tc.status, tc.message)
			}
		})
	}
	t.Run("exact threshold", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			// synctest holds the clock at UTC midnight, so equality is exact
			// and does not depend on how fast the check runs.
			date := time.Now().UTC().Add(180 * 24 * time.Hour).Format("2006-01-02")
			nodeFiles(t, map[string]string{
				"etc/os-release":                   "ID=debian\nVERSION_CODENAME=trixie\nPRETTY_NAME=Test Linux\n",
				"usr/share/distro-info/debian.csv": debianHeader + "13,Trixie,trixie,,," + date + "\n",
			})
			inputs := &checksandalerts.DistributionReleaseSecuritySupport{SecuritySupport: "regular", FailWhenSecuritySupportEndsWithin: "180d"}
			check := checksandalerts.Check{Inputs: inputs}
			if o := withinTimeLimit(t, 5*time.Second, check); o.Status != "failed" {
				t.Fatalf("at the threshold: %v", o)
			}
			inputs.FailWhenSecuritySupportEndsWithin = checksandalerts.Duration((180*24*time.Hour - time.Nanosecond).String())
			if o := withinTimeLimit(t, 5*time.Second, check); o.Status != "ok" {
				t.Fatalf("outside the threshold: %v", o)
			}
		})
	})
}

// osvStandIn stands in for CISA's list and for OSV.dev on one test server.
// It answers a batch as OSV.dev does: in the order of the queries, at most
// idsPerPage flaw IDs for each query on one page with a token for the next
// page, an empty object for a query without flaws, and HTTP 400 with "too
// many queries" for more than 1,000 queries. It records each batch and
// each record that the check asks for.
type osvStandIn struct {
	t          *testing.T
	guard      sync.Mutex
	cisaStatus int
	cisaBody   string
	// batchStatus and batchBody replace OSV.dev's answer to a batch when
	// batchStatus is not 0.
	batchStatus int
	batchBody   string
	// flaws holds the flaw IDs of each ecosystem and source package.
	flaws      map[string]map[string][]string
	records    map[string]string
	idsPerPage int
	batches    []osvBatch
	fetched    []string
}

// osvBatch is one batch that the check sent, read strictly: a field other
// than a package's name, its ecosystem, and a page token fails the test.
type osvBatch struct {
	Queries []struct {
		Package struct {
			Name      string `json:"name"`
			Ecosystem string `json:"ecosystem"`
		} `json:"package"`
		PageToken string `json:"page_token"`
	} `json:"queries"`
}

func newOSVStandIn(t *testing.T) *osvStandIn {
	t.Helper()
	standIn := &osvStandIn{t: t, cisaStatus: 200, idsPerPage: 2}
	server := httptest.NewServer(standIn)
	t.Cleanup(server.Close)
	for variable, value := range map[*string]string{checksandalerts.CISAKnownExploitedVulnerabilitiesURLForTest: server.URL + "/known_exploited_vulnerabilities.json", checksandalerts.OSVAddressForTest: server.URL} {
		original := *variable
		*variable = value
		t.Cleanup(func() { *variable = original })
	}
	return standIn
}

func (s *osvStandIn) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.guard.Lock()
	defer s.guard.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.Method == "GET" && r.URL.Path == "/known_exploited_vulnerabilities.json":
		w.WriteHeader(s.cisaStatus)
		io.WriteString(w, s.cisaBody)
	case r.Method == "POST" && r.URL.Path == "/v1/querybatch" && r.Header.Get("Content-Type") == "application/json":
		var batch osvBatch
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&batch); err != nil {
			s.t.Errorf("a batch that holds more than names: %v", err)
			w.WriteHeader(400)
			return
		}
		s.batches = append(s.batches, batch)
		if s.batchStatus != 0 {
			w.WriteHeader(s.batchStatus)
			io.WriteString(w, s.batchBody)
			return
		}
		if len(batch.Queries) > 1000 {
			w.WriteHeader(400)
			io.WriteString(w, `{"code":3,"message":"too many queries"}`)
			return
		}
		results := []map[string]any{}
		for _, query := range batch.Queries {
			ids, start := s.flaws[query.Package.Ecosystem][query.Package.Name], 0
			if query.PageToken != "" {
				start, _ = strconv.Atoi(strings.TrimPrefix(query.PageToken, "after-"))
			}
			end, result := min(start+s.idsPerPage, len(ids)), map[string]any{}
			if end > start {
				vulns := []map[string]string{}
				for _, id := range ids[start:end] {
					vulns = append(vulns, map[string]string{"id": id, "modified": "2026-10-26T07:05:12Z"})
				}
				result["vulns"] = vulns
			}
			if end < len(ids) {
				result["next_page_token"] = fmt.Sprintf("after-%d", end)
			}
			results = append(results, result)
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/vulns/"):
		id := strings.TrimPrefix(r.URL.Path, "/v1/vulns/")
		s.fetched = append(s.fetched, id)
		record, found := s.records[id]
		if !found {
			w.WriteHeader(404)
			io.WriteString(w, `{"code":5,"message":"Bug not found."}`)
			return
		}
		io.WriteString(w, record)
	default:
		s.t.Errorf("unexpected request: %s %s %v", r.Method, r.URL, r.Header)
		w.WriteHeader(404)
	}
}

// set changes the stand-in's answers between runs.
func (s *osvStandIn) set(change func()) {
	s.guard.Lock()
	defer s.guard.Unlock()
	change()
}

// asked returns the batches and the records that the check asked for
// since the call before.
func (s *osvStandIn) asked() ([]osvBatch, []string) {
	s.guard.Lock()
	defer s.guard.Unlock()
	batches, fetched := s.batches, s.fetched
	s.batches, s.fetched = nil, nil
	return batches, fetched
}

// queriesOf lists the queries of batches as "ecosystem name" or
// "ecosystem name page_token", sorted.
func queriesOf(batches []osvBatch) []string {
	queries := []string{}
	for _, batch := range batches {
		for _, query := range batch.Queries {
			queries = append(queries, strings.TrimSpace(query.Package.Ecosystem+" "+query.Package.Name+" "+query.PageToken))
		}
	}
	slices.Sort(queries)
	return queries
}

// cisaList is CISA's list of known exploited vulnerabilities with the CVEs
// given, each named "Test flaw" and its CVE.
func cisaList(cves ...string) string {
	entries := []map[string]string{}
	for _, cve := range cves {
		entries = append(entries, map[string]string{"cveID": cve, "vendorProject": "Test", "product": "Test", "vulnerabilityName": "Test flaw " + cve, "dateAdded": "2026-10-01"})
	}
	body, _ := json.Marshal(map[string]any{"title": "CISA Catalog of Known Exploited Vulnerabilities", "catalogVersion": "2026.10.26", "dateReleased": "2026-10-26T22:24:24.6101Z", "count": len(entries), "vulnerabilities": entries})
	return string(body)
}

// osvEntry is one affected entry of an OSV.dev record: one source package
// in one ecosystem, with the events of its range of type ECOSYSTEM.
type osvEntry struct {
	source, ecosystem string
	events            []map[string]string
}

func osvFixedIn(version string) []map[string]string {
	return []map[string]string{{"introduced": "0"}, {"fixed": version}}
}

var osvNoFixYet = []map[string]string{{"introduced": "0"}}

func osvRecord(id string, entries ...osvEntry) string {
	affected := []map[string]any{}
	for _, entry := range entries {
		affected = append(affected, map[string]any{"package": map[string]string{"name": entry.source, "ecosystem": entry.ecosystem},
			"ranges": []map[string]any{{"type": "ECOSYSTEM", "events": entry.events}}})
	}
	body, _ := json.Marshal(map[string]any{"id": id, "modified": "2026-10-26T07:05:12Z", "upstream": []string{strings.TrimPrefix(strings.TrimPrefix(id, "DEBIAN-"), "UBUNTU-")}, "affected": affected})
	return string(body)
}

// A known_exploited_vulnerabilities_in_installed_packages check reads the
// node's packages with dpkg-query and apt-cache, downloads CISA's list, asks
// OSV.dev about the source package names only, page by page, and compares
// the versions on the node.
func TestKnownExploitedVulnerabilitiesInInstalledPackagesCheck(t *testing.T) {
	ranAs := asThisAccount(t)
	root := nodeFiles(t, map[string]string{"etc/os-release": "ID=debian\nVERSION_ID=\"13\"\nVERSION=\"13 (trixie)\"\n", "proc/sys/kernel/osrelease": "6.12.113-amd64\n",
		"proc/version": "Linux version 6.12.113-amd64 (debian-kernel@lists.debian.org) (x86_64-linux-gnu-gcc-14 (Debian 14.2.0-19) 14.2.0, GNU ld (GNU Binutils for Debian) 2.44) #1 SMP PREEMPT_DYNAMIC Debian 6.12.113-1 (2026-10-15)\n"})
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	check := &checksandalerts.KnownExploitedVulnerabilitiesInInstalledPackages{FailWhen: "an_installed_package_is_affected"}
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: check})
	}
	packages, packageExit, packageAsked := clientProgram(t, "dpkg-query", "LC_ALL")
	const installed = "installed\tsudo\tsudo\t1.9.16p2-2\t1.9.16p2-2\t\n" +
		"installed\topenssh-client\topenssh\t1:9.9-1\t1:9.9-1\t\n" +
		"installed\topenssh-server\topenssh\t1:9.8-1\t1:9.8-1\t\n" +
		"installed\ttailscale\ttailscale\t1.100\t1.100\t\n" +
		"installed\tcaddy\tcaddy\t2.0\t2.0\t\n" +
		"installed\tbackported\tbackported\t3.0~bpo13\t3.0~bpo13\t\n" +
		"installed\tlinux-image-6.12.113-amd64\tlinux-signed-amd64\t6.12.113+1\t6.12.113-1\tlinux (= 6.12.113-1), other (= 1.0)\n" +
		"installed\tlinux-headers-old\tlinux\t6.12.1-1\t6.12.1-1\t\n" +
		"installed\tlinux-modules-old\tlinux\t6.12.0-1\t6.12.0-1\t\n" +
		"installed\tsudo\tsudo\t1.9.16p2-2\t1.9.16p2-2\t\n" +
		"config-files\tremoved\tremoved\t1.0\t1.0\t\n"
	write(packages, installed)
	policy, madison, aptAsked := filepath.Join(t.TempDir(), "policy"), filepath.Join(t.TempDir(), "madison"), filepath.Join(t.TempDir(), "asked")
	write(policy, " 500 http://deb.debian.org/debian trixie/main amd64 Packages\n     release v=13,o=Debian,a=stable,n=trixie,c=main,b=amd64\n"+
		" 500 https://vendor.example.com/stable trixie/main amd64 Packages\n     release o=Test Vendor With Spaces,a=stable\n"+
		" 100 http://deb.debian.org/debian trixie-backports/main amd64 Packages\n     release o=Debian Backports,a=stable-backports\n")
	write(madison, "sudo | 1.9.16p2-3 | http://deb.debian.org/debian trixie/main amd64 Packages\n"+
		"caddy | 2.0 | http://deb.debian.org/debian trixie/main amd64 Packages\n"+
		"caddy | 99.0 | https://vendor.example.com/stable trixie/main amd64 Packages\n"+
		"tailscale | 1.104.2 | https://vendor.example.com/stable trixie/main amd64 Packages\n"+
		"tailscale | 1.100 | http://deb.debian.org/debian trixie/main Sources\n"+
		"backported | 3.0~bpo13 | http://deb.debian.org/debian trixie-backports/main amd64 Packages\n")
	fakeProgram(t, "apt-cache", "printf '%s\\n' \"$@\" >> "+testhelpers.ShellQuote(aptAsked)+"\nif [ \"$1\" = policy ]; then cat "+testhelpers.ShellQuote(policy)+"; else cat "+testhelpers.ShellQuote(madison)+"; fi")

	// Each event rule of OSV.dev decides one flaw: a fixed version at or
	// below the installed one, an introduced version above it, and a
	// last_affected version below it leave it unaffected; a last_affected
	// version equal to it does not.
	standIn := newOSVStandIn(t)
	cisa := cisaList("CVE-2021-3156", "CVE-2024-6387", "CVE-2025-32463", "CVE-2026-1111", "CVE-2026-1112", "CVE-2026-1113", "CVE-2026-2222", "CVE-2026-3333", "CVE-2026-4444", "CVE-2026-7777")
	flaws := map[string]map[string][]string{"Debian:13": {
		"sudo":    {"DEBIAN-CVE-2021-3156", "DEBIAN-CVE-2025-32463", "DEBIAN-CVE-2026-9000", "DSA-6000-1"},
		"openssh": {"DEBIAN-CVE-2024-6387", "DEBIAN-CVE-2026-1111", "DEBIAN-CVE-2026-1112", "DEBIAN-CVE-2026-1113"},
		"caddy":   {"DEBIAN-CVE-2026-2222"},
		// The flaw on CISA's list is on the third page.
		"linux": {"DEBIAN-CVE-2026-9001", "DEBIAN-CVE-2026-4444", "DEBIAN-CVE-2026-9002", "DEBIAN-CVE-2026-9003", "DEBIAN-CVE-2026-3333"},
	}}
	records := map[string]string{
		"DEBIAN-CVE-2021-3156":  osvRecord("DEBIAN-CVE-2021-3156", osvEntry{"sudo", "Debian:12", osvFixedIn("1.9.5p1-1.1")}, osvEntry{"sudo", "Debian:13", osvFixedIn("1.9.5p1-1.1")}),
		"DEBIAN-CVE-2025-32463": osvRecord("DEBIAN-CVE-2025-32463", osvEntry{"sudo", "Debian:12", osvFixedIn("1.9.13p3-1+deb12u2")}, osvEntry{"sudo", "Debian:13", osvFixedIn("1.9.16p2-3")}),
		"DEBIAN-CVE-2024-6387":  osvRecord("DEBIAN-CVE-2024-6387", osvEntry{"openssh", "Debian:13", osvFixedIn("1:9.7p1-7")}),
		"DEBIAN-CVE-2026-1111":  osvRecord("DEBIAN-CVE-2026-1111", osvEntry{"openssh", "Debian:13", []map[string]string{{"introduced": "0"}, {"last_affected": "1:9.8-1"}}}),
		"DEBIAN-CVE-2026-1112":  osvRecord("DEBIAN-CVE-2026-1112", osvEntry{"openssh", "Debian:13", []map[string]string{{"introduced": "0"}, {"last_affected": "1:9.7-1"}}}),
		"DEBIAN-CVE-2026-1113":  osvRecord("DEBIAN-CVE-2026-1113", osvEntry{"openssh", "Debian:13", []map[string]string{{"introduced": "1:9.9-1"}, {"fixed": "1:9.9-2"}}}),
		"DEBIAN-CVE-2026-2222":  osvRecord("DEBIAN-CVE-2026-2222", osvEntry{"caddy", "Debian:13", osvNoFixYet}),
		"DEBIAN-CVE-2026-3333":  osvRecord("DEBIAN-CVE-2026-3333", osvEntry{"linux", "Debian:13", osvFixedIn("6.12.114-1")}),
		// An older installed module must not make the booted kernel fail.
		"DEBIAN-CVE-2026-4444": osvRecord("DEBIAN-CVE-2026-4444", osvEntry{"linux", "Debian:13", osvFixedIn("6.12.113-1")}),
	}
	reset := func() {
		standIn.set(func() {
			standIn.cisaStatus, standIn.cisaBody, standIn.batchStatus, standIn.batchBody = 200, cisa, 0, ""
			standIn.flaws, standIn.records = flaws, maps.Clone(records)
		})
	}
	reset()
	o := observe()
	wantFindings := []checksandalerts.Finding{
		{ID: "CVE-2026-2222 caddy", Message: "CVE-2026-2222 in caddy 2.0: no fix yet (CISA: Test flaw CVE-2026-2222; https://security-tracker.debian.org/tracker/CVE-2026-2222)",
			ExactFinding: "Debian:13 CVE-2026-2222 caddy 2.0; fixed version: none; DEBIAN-CVE-2026-2222 modified 2026-10-26T07:05:12Z"},
		{ID: "CVE-2026-3333 linux", Message: "CVE-2026-3333 in linux 6.12.113-1, the running kernel: fixed version 6.12.114-1 waits to be installed (CISA: Test flaw CVE-2026-3333; https://security-tracker.debian.org/tracker/CVE-2026-3333)",
			ExactFinding: "Debian:13 CVE-2026-3333 linux 6.12.113-1; fixed version waits: 6.12.114-1; DEBIAN-CVE-2026-3333 modified 2026-10-26T07:05:12Z"},
		{ID: "CVE-2026-1111 openssh", Message: "CVE-2026-1111 in openssh 1:9.8-1: no fix yet (CISA: Test flaw CVE-2026-1111; https://security-tracker.debian.org/tracker/CVE-2026-1111)",
			ExactFinding: "Debian:13 CVE-2026-1111 openssh 1:9.8-1; fixed version: none; DEBIAN-CVE-2026-1111 modified 2026-10-26T07:05:12Z"},
		{ID: "CVE-2025-32463 sudo", Message: "CVE-2025-32463 in sudo 1.9.16p2-2: fixed version 1.9.16p2-3 waits to be installed (CISA: Test flaw CVE-2025-32463; https://security-tracker.debian.org/tracker/CVE-2025-32463)",
			ExactFinding: "Debian:13 CVE-2025-32463 sudo 1.9.16p2-2; fixed version waits: 1.9.16p2-3; DEBIAN-CVE-2025-32463 modified 2026-10-26T07:05:12Z"},
	}
	if o.Status != "failed" || o.Message != "4 known exploited flaws affect installed packages: CVE-2026-2222 caddy, CVE-2026-3333 linux, CVE-2026-1111 openssh, CVE-2025-32463 sudo" || !reflect.DeepEqual(o.Findings, wantFindings) {
		t.Fatalf("%+v", o)
	}
	// Names only: the first round asks each source package once, and each
	// later round asks a package again with OSV.dev's token for its next
	// page. The vendor's tailscale and the backport are left out.
	batches, fetched := standIn.asked()
	if got := queriesOf(batches[:1]); !reflect.DeepEqual(got, []string{"Debian:13 caddy", "Debian:13 linux", "Debian:13 openssh", "Debian:13 sudo"}) {
		t.Fatalf("first round: %q", got)
	}
	if got := queriesOf(batches[1:]); !reflect.DeepEqual(got, []string{"Debian:13 linux after-2", "Debian:13 linux after-4", "Debian:13 openssh after-2", "Debian:13 sudo after-2"}) {
		t.Fatalf("later rounds: %q", got)
	}
	// Each record of a flaw on CISA's list is fetched once; flaws that are
	// not on the list and advisories are not.
	slices.Sort(fetched)
	if want := []string{"DEBIAN-CVE-2021-3156", "DEBIAN-CVE-2024-6387", "DEBIAN-CVE-2025-32463", "DEBIAN-CVE-2026-1111", "DEBIAN-CVE-2026-1112", "DEBIAN-CVE-2026-1113", "DEBIAN-CVE-2026-2222", "DEBIAN-CVE-2026-3333", "DEBIAN-CVE-2026-4444"}; !reflect.DeepEqual(fetched, want) {
		t.Fatalf("fetched records: %q", fetched)
	}
	if got, _ := os.ReadFile(packageAsked); string(got) != "--show\n--showformat=${db:Status-Status}\t${Package}\t${source:Package}\t${source:Version}\t${Version}\t${Built-Using}\n\nLC_ALL=C\n" {
		t.Fatalf("dpkg arguments: %q", got)
	}
	if got := ranAs(); !reflect.DeepEqual(got, []string{"mmwatchdog", "mmwatchdog", "mmwatchdog"}) {
		t.Fatal(got)
	}
	if got, _ := os.ReadFile(aptAsked); string(got) != "policy\nmadison\nbackported\ncaddy\nlinux-headers-old\nlinux-image-6.12.113-amd64\nlinux-modules-old\nopenssh-client\nopenssh-server\nsudo\ntailscale\n" {
		t.Fatalf("apt arguments: %q", got)
	}
	if got := checksandalerts.KnownExploitedVulnerabilityIsGoneForTest("CVE-2025-32463 sudo"); got != "CVE-2025-32463 no longer affects sudo on this node" {
		t.Fatal(got)
	}

	// A new assessment of the flaw by the distribution changes its record:
	// the exact finding changes with it, and the finding stays the same.
	standIn.set(func() {
		standIn.records["DEBIAN-CVE-2026-2222"] = strings.Replace(records["DEBIAN-CVE-2026-2222"], "2026-10-26T07:05:12Z", "2026-11-20T08:12:00Z", 1)
	})
	if reassessed := observe(); len(reassessed.Findings) != 4 || reassessed.Findings[0].ID != wantFindings[0].ID ||
		reassessed.Findings[0].ExactFinding != "Debian:13 CVE-2026-2222 caddy 2.0; fixed version: none; DEBIAN-CVE-2026-2222 modified 2026-11-20T08:12:00Z" {
		t.Fatalf("%+v", reassessed)
	}
	reset()

	// The only input is required and has one value.
	for _, failWhen := range []string{"", "another_rule"} {
		inputs := checksandalerts.KnownExploitedVulnerabilitiesInInstalledPackages{FailWhen: failWhen}
		if problem := inputs.Validate(checksandalerts.InputValidation{}); problem == nil || problem.Path != "fail_when" {
			t.Fatal(failWhen, problem)
		}
	}
	if problem := check.Validate(checksandalerts.InputValidation{}); problem != nil {
		t.Fatal(problem)
	}

	// Healthy: no flaw on CISA's list affects an installed version.
	standIn.set(func() {
		standIn.flaws = map[string]map[string][]string{"Debian:13": {"sudo": {"DEBIAN-CVE-2021-3156", "DEBIAN-CVE-2026-9000"}}}
	})
	const healthy = "no installed package has a flaw on CISA's list of known exploited vulnerabilities of 2026-10-26: MMWatchdog searched OSV.dev for 4 source packages of Debian:13"
	if o := observe(); o.Status != "ok" || o.Message != healthy || len(o.Findings) != 0 {
		t.Fatal(o)
	}

	// CISA's list is read up to 16 MiB: today's list is larger than the
	// 1 MiB that HTTP requests read by default.
	standIn.set(func() { standIn.cisaBody = cisa + strings.Repeat(" ", 2*primitives.HTTPResponseBodyLimit) })
	if o := observe(); o.Status != "ok" || o.Message != healthy {
		t.Fatal(o)
	}
	standIn.set(func() { standIn.cisaBody = cisa + strings.Repeat(" ", 16*primitives.HTTPResponseBodyLimit) })
	if o := observe(); o.Status != "unknown" || o.Message != "CISA's list of known exploited vulnerabilities cannot be read: the response body exceeds 16 MiB" {
		t.Fatal(o)
	}

	// Every answer that cannot be used is unknown, never healthy, and says
	// which service gave it.
	for _, test := range []struct {
		name    string
		change  func()
		message string
	}{
		{"CISA's error", func() { standIn.cisaStatus = 503 }, "raw.githubusercontent.com answered HTTP 503 for CISA's list of known exploited vulnerabilities"},
		{"CISA's list is not JSON", func() { standIn.cisaBody = "not JSON" }, "CISA's list of known exploited vulnerabilities cannot be read: invalid character 'o' in literal null (expecting 'u')"},
		{"CISA's list is empty", func() { standIn.cisaBody = `{"dateReleased":"2026-10-26T22:24:24.6101Z","vulnerabilities":[]}` }, "CISA's list of known exploited vulnerabilities cannot be read: expected dateReleased and vulnerabilities"},
		{"CISA's list has no date", func() { standIn.cisaBody = `{"vulnerabilities":[{"cveID":"CVE-2025-32463"}]}` }, "CISA's list of known exploited vulnerabilities cannot be read: expected dateReleased and vulnerabilities"},
		{"an entry without a CVE", func() {
			standIn.cisaBody = `{"dateReleased":"2026-10-26T22:24:24.6101Z","vulnerabilities":[{"cveID":"CVE-2025-32463"},{"vulnerabilityName":"A flaw"}]}`
		}, "CISA's list of known exploited vulnerabilities cannot be read: an entry has no cveID"},
		{"OSV.dev's error", func() { standIn.batchStatus, standIn.batchBody = 500, `{"code":13,"message":"internal"}` }, "api.osv.dev answered HTTP 500 for OSV.dev's flaws of the installed source packages"},
		{"OSV.dev's answer is not JSON", func() { standIn.batchStatus, standIn.batchBody = 200, "not JSON" }, "OSV.dev's flaws of the installed source packages cannot be read: invalid character 'o' in literal null (expecting 'u')"},
		{"OSV.dev answers fewer queries", func() { standIn.batchStatus, standIn.batchBody = 200, `{"results":[{}]}` }, "OSV.dev's flaws of the installed source packages cannot be read: expected 4 results, received 1"},
		{"a record is missing", func() { delete(standIn.records, "DEBIAN-CVE-2025-32463") }, "api.osv.dev answered HTTP 404 for OSV.dev's record DEBIAN-CVE-2025-32463"},
		{"a record without the release", func() {
			standIn.records["DEBIAN-CVE-2025-32463"] = osvRecord("DEBIAN-CVE-2025-32463", osvEntry{"sudo", "Debian:12", osvFixedIn("1.9.13p3-1+deb12u2")})
		}, "OSV.dev's record DEBIAN-CVE-2025-32463 cannot be read: it has no entry for sudo in Debian:13"},
		{"a record without a range of the release's versions", func() {
			standIn.records["DEBIAN-CVE-2025-32463"] = `{"id":"DEBIAN-CVE-2025-32463","affected":[{"package":{"name":"sudo","ecosystem":"Debian:13"},"ranges":[{"type":"GIT","repo":"https://example.com/sudo","events":[{"introduced":"0"}]}]}]}`
		}, "OSV.dev's record DEBIAN-CVE-2025-32463 cannot be read: its entry for sudo in Debian:13 has no range of type ECOSYSTEM"},
		// OSV.dev answers a release that it does not know as one without
		// flaws: no flaw at all for any package is unknown, not healthy.
		{"OSV.dev knows no flaw of the release", func() { standIn.flaws = map[string]map[string][]string{} }, "OSV.dev knows no flaw of any of the 4 source packages in Debian:13, so it may not cover this release"},
	} {
		t.Run(test.name, func(t *testing.T) {
			reset()
			standIn.set(test.change)
			if o := observe(); o.Status != "unknown" || o.Message != test.message || len(o.Findings) != 0 {
				t.Fatalf("got %+v, want unknown: %s", o, test.message)
			}
		})
	}
	reset()

	// A newer installed kernel that a flaw does not affect runs only after
	// a reboot: the finding names that kernel and the reboot instead of an
	// install. A newer installed kernel that the flaw still affects changes
	// nothing.
	write(packages, installed+"installed\tlinux-image-6.12.115-amd64\tlinux-signed-amd64\t6.12.115+1\t6.12.115-1\tlinux (= 6.12.115-1)\n")
	if o := observe(); o.Status != "failed" || len(o.Findings) != 4 || o.Findings[1].Message != "CVE-2026-3333 in linux 6.12.113-1, the running kernel: the fixed kernel 6.12.115-1 is installed and runs after a reboot (CISA: Test flaw CVE-2026-3333; https://security-tracker.debian.org/tracker/CVE-2026-3333)" ||
		o.Findings[1].ExactFinding != "Debian:13 CVE-2026-3333 linux 6.12.113-1; fixed kernel installed, runs after a reboot: 6.12.115-1; DEBIAN-CVE-2026-3333 modified 2026-10-26T07:05:12Z" {
		t.Fatalf("%+v", o)
	}
	write(packages, installed+"installed\tlinux-image-6.12.113.5-amd64\tlinux-signed-amd64\t6.12.113.5+1\t6.12.113.5-1\tlinux (= 6.12.113.5-1)\n")
	if o := observe(); o.Status != "failed" || len(o.Findings) != 4 || o.Findings[1].Message != wantFindings[1].Message {
		t.Fatalf("%+v", o)
	}
	// Debian can update the running kernel's image under its package name:
	// the node runs the version that /proc/version names until it reboots,
	// and the installed version is the fixed kernel.
	write(packages, strings.Replace(installed, "6.12.113+1\t6.12.113-1\tlinux (= 6.12.113-1)", "6.12.115+1\t6.12.115-1\tlinux (= 6.12.115-1)", 1))
	if o := observe(); o.Status != "failed" || len(o.Findings) != 4 || o.Findings[1].Message != "CVE-2026-3333 in linux 6.12.113-1, the running kernel: the fixed kernel 6.12.115-1 is installed and runs after a reboot (CISA: Test flaw CVE-2026-3333; https://security-tracker.debian.org/tracker/CVE-2026-3333)" ||
		o.Findings[1].ExactFinding != "Debian:13 CVE-2026-3333 linux 6.12.113-1; fixed kernel installed, runs after a reboot: 6.12.115-1; DEBIAN-CVE-2026-3333 modified 2026-10-26T07:05:12Z" {
		t.Fatalf("%+v", o)
	}
	write(packages, installed)

	// A running kernel that is not from a package is left out, and the
	// message says so.
	write(filepath.Join(root, "proc/sys/kernel/osrelease"), "not-packaged\n")
	standIn.asked()
	if o := observe(); o.Status != "failed" || len(o.Findings) != 3 || o.Findings[0].ID != "CVE-2026-2222 caddy" || !strings.HasSuffix(o.Message, "; the running kernel not-packaged is not from a package, so its flaws were not looked for") {
		t.Fatal(o)
	}
	if batches, _ := standIn.asked(); !reflect.DeepEqual(queriesOf(batches[:1]), []string{"Debian:13 caddy", "Debian:13 openssh", "Debian:13 sudo"}) {
		t.Fatal(queriesOf(batches))
	}
	standIn.set(func() {
		standIn.flaws = map[string]map[string][]string{"Debian:13": {"sudo": {"DEBIAN-CVE-2021-3156"}}}
	})
	if o := observe(); o.Status != "ok" || o.Message != strings.Replace(healthy, "for 4", "for 3", 1)+"; the running kernel not-packaged is not from a package, so its flaws were not looked for" {
		t.Fatal(o)
	}
	reset()

	for _, origin := range []string{"Debian", "Ubuntu", "UbuntuESM", "UbuntuESMApps"} {
		t.Run(origin+" origin", func(t *testing.T) {
			write(policy, "500 http://archive.example.com stable/main amd64 Packages\n release o="+origin+",a=stable\n500 https://vendor.example.com stable/main amd64 Packages\n release o=A Vendor\n")
			write(madison, "sudo | 1.9.16p2-2 | http://archive.example.com stable/main amd64 Packages\nsudo | 99.0 | https://vendor.example.com stable/main amd64 Packages\n")
			if o := observe(); o.Status != "failed" || !slices.ContainsFunc(o.Findings, func(f checksandalerts.Finding) bool { return f.ID == "CVE-2025-32463 sudo" }) {
				t.Fatal(o)
			}
		})
	}

	// The binary's epoch belongs to apt origin matching, not the CVE comparison.
	// Both archives offer bsdutils; only the distribution offers its installed binary version.
	write(packages, "installed\tbsdutils\tutil-linux\t2.39.3-9ubuntu6.6\t1:2.39.3-9ubuntu6.6\t\n")
	write(madison, "bsdutils | 1:2.39.3-9ubuntu6.6 | http://archive.example.com stable/main amd64 Packages\nbsdutils | 99.0 | https://vendor.example.com stable/main amd64 Packages\n")
	standIn.set(func() {
		standIn.flaws = map[string]map[string][]string{"Debian:13": {"util-linux": {"DEBIAN-CVE-2026-7777"}}}
		standIn.records["DEBIAN-CVE-2026-7777"] = osvRecord("DEBIAN-CVE-2026-7777", osvEntry{"util-linux", "Debian:13", osvFixedIn("2.39.3-9ubuntu6.6+test1")})
	})
	standIn.asked()
	if o := observe(); o.Status != "failed" || len(o.Findings) != 1 || o.Findings[0].Message != "CVE-2026-7777 in util-linux 2.39.3-9ubuntu6.6: fixed version 2.39.3-9ubuntu6.6+test1 waits to be installed (CISA: Test flaw CVE-2026-7777; https://security-tracker.debian.org/tracker/CVE-2026-7777)" {
		t.Fatal(o)
	}
	if batches, _ := standIn.asked(); !reflect.DeepEqual(queriesOf(batches), []string{"Debian:13 util-linux"}) {
		t.Fatal(queriesOf(batches))
	}

	// A container can have the kernel's userspace headers without an image.
	write(packages, "installed\tlinux-libc-dev\tlinux\t6.12.1-1\t6.12.1-1\t\ninstalled\tsudo\tsudo\t1.9.16p2-2\t1.9.16p2-2\t\n")
	reset()
	if o := observe(); o.Status != "failed" || len(o.Findings) != 1 || o.Findings[0].ID != "CVE-2025-32463 sudo" {
		t.Fatal(o)
	}

	// An unsigned running image falls back to its own source and version.
	write(filepath.Join(root, "proc/sys/kernel/osrelease"), "unsigned\n")
	write(packages, "installed\tlinux-image-unsigned\tlinux-custom\t6.12.113-1\t6.12.113-1\t\n")
	standIn.set(func() {
		standIn.flaws = map[string]map[string][]string{"Debian:13": {"linux-custom": {"DEBIAN-CVE-2026-3333"}}}
		standIn.records["DEBIAN-CVE-2026-3333"] = osvRecord("DEBIAN-CVE-2026-3333", osvEntry{"linux-custom", "Debian:13", osvFixedIn("6.12.114-1")})
	})
	if o := observe(); o.Status != "failed" || len(o.Findings) != 1 || !strings.Contains(o.Findings[0].Message, "in linux-custom 6.12.113-1, the running kernel:") {
		t.Fatal(o)
	}
	write(packages, installed)
	reset()

	write(packageExit, "1")
	write(packages+".stderr", "dpkg database unavailable\n")
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot read installed packages with dpkg-query: dpkg database unavailable" {
		t.Fatal(o)
	}
	write(packageExit, "0")

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	refusedAddress := listener.Addr().String()
	listener.Close()
	original := *checksandalerts.OSVAddressForTest
	*checksandalerts.OSVAddressForTest = "http://" + refusedAddress
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot reach api.osv.dev for OSV.dev's flaws of the installed source packages: dial tcp "+refusedAddress+": connect: connection refused" {
		t.Fatal(o)
	}
	*checksandalerts.OSVAddressForTest = original
	*checksandalerts.CISAKnownExploitedVulnerabilitiesURLForTest = "http://" + refusedAddress + "/known_exploited_vulnerabilities.json"
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot reach raw.githubusercontent.com for CISA's list of known exploited vulnerabilities: dial tcp "+refusedAddress+": connect: connection refused" {
		t.Fatal(o)
	}
}

// On an Ubuntu LTS release, the check asks OSV.dev about the release's
// ecosystem and its Ubuntu Pro ecosystem, where Ubuntu files some flaws
// only. An installed fix of either one is healthy, and a fix of the
// release goes before one of Ubuntu Pro. An interim release has no Ubuntu
// Pro ecosystem.
func TestKnownExploitedVulnerabilitiesInBothUbuntuEcosystems(t *testing.T) {
	asThisAccount(t)
	release := filepath.Join(nodeFiles(t, map[string]string{"etc/os-release": "ID=ubuntu\nVERSION_ID=\"24.04\"\nVERSION=\"24.04.3 LTS (Noble Numbat)\"\n", "proc/sys/kernel/osrelease": "6.8.0-87-generic\n",
		"proc/version": "Linux version 6.8.0-87-generic (buildd@lcy02-amd64-081) (x86_64-linux-gnu-gcc-13 (Ubuntu 13.3.0-6ubuntu2~24.04.1) 13.3.0, GNU ld (GNU Binutils for Ubuntu) 2.42) #88-Ubuntu SMP PREEMPT_DYNAMIC Sat Oct 11 09:28:41 UTC 2025\n"}), "etc/os-release")
	packages, _, _ := clientProgram(t, "dpkg-query", "LC_ALL")
	if err := os.WriteFile(packages, []byte("installed\troundcube\troundcube\t1.6.6+dfsg-2ubuntu0.1\t1.6.6+dfsg-2ubuntu0.1\t\n"+
		"installed\topenssl\topenssl\t3.0.13-0ubuntu3.5\t3.0.13-0ubuntu3.5\t\n"+
		"installed\tsudo\tsudo\t1.9.15p5-3ubuntu5.24.04.1\t1.9.15p5-3ubuntu5.24.04.1\t\n"+
		"installed\tlinux-image-6.8.0-87-generic\tlinux-signed\t6.8.0-87.88\t6.8.0-87.88\tlinux (= 6.8.0-87.88)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fakeProgram(t, "apt-cache", "true")
	check := checksandalerts.Check{Inputs: &checksandalerts.KnownExploitedVulnerabilitiesInInstalledPackages{FailWhen: "an_installed_package_is_affected"}}
	const main, pro = "Ubuntu:24.04:LTS", "Ubuntu:Pro:24.04:LTS"
	standIn := newOSVStandIn(t)
	standIn.set(func() {
		standIn.cisaBody = cisaList("CVE-2024-42009", "CVE-2025-32463", "CVE-2026-5555", "CVE-2026-6666")
		standIn.flaws = map[string]map[string][]string{
			main: {"openssl": {"UBUNTU-CVE-2026-5555"}, "sudo": {"UBUNTU-CVE-2025-32463", "USN-7604-1"}, "linux": {"UBUNTU-CVE-2026-6666"}},
			pro:  {"roundcube": {"UBUNTU-CVE-2024-42009"}, "openssl": {"UBUNTU-CVE-2026-5555"}, "sudo": {"UBUNTU-CVE-2025-32463"}, "linux": {"LSN-0110-1"}},
		}
		standIn.records = map[string]string{
			// Only Ubuntu Pro has the fix.
			"UBUNTU-CVE-2024-42009": osvRecord("UBUNTU-CVE-2024-42009", osvEntry{"roundcube", "Ubuntu:Pro:22.04:LTS", osvFixedIn("1.5.0+dfsg.1-2ubuntu0.1~esm4")}, osvEntry{"roundcube", pro, osvFixedIn("1.6.6+dfsg-2ubuntu0.1+esm1")}),
			// The installed version is Ubuntu Pro's fix.
			"UBUNTU-CVE-2026-5555": osvRecord("UBUNTU-CVE-2026-5555", osvEntry{"openssl", main, osvNoFixYet}, osvEntry{"openssl", pro, osvFixedIn("3.0.13-0ubuntu3.5")}),
			// Both have a fix: the release's own goes first.
			"UBUNTU-CVE-2025-32463": osvRecord("UBUNTU-CVE-2025-32463", osvEntry{"sudo", main, osvFixedIn("1.9.15p5-3ubuntu5.24.04.2")}, osvEntry{"sudo", pro, osvFixedIn("1.9.15p5-3ubuntu5.24.04.1+esm1")}),
			"UBUNTU-CVE-2026-6666":  osvRecord("UBUNTU-CVE-2026-6666", osvEntry{"linux", main, osvNoFixYet}),
		}
	})
	o := withinTimeLimit(t, 5*time.Second, check)
	wantFindings := []checksandalerts.Finding{
		{ID: "CVE-2026-6666 linux", Message: "CVE-2026-6666 in linux 6.8.0-87.88, the running kernel: no fix yet (CISA: Test flaw CVE-2026-6666; https://ubuntu.com/security/CVE-2026-6666)",
			ExactFinding: "Ubuntu:24.04:LTS CVE-2026-6666 linux 6.8.0-87.88; fixed version: none; UBUNTU-CVE-2026-6666 modified 2026-10-26T07:05:12Z"},
		{ID: "CVE-2024-42009 roundcube", Message: "CVE-2024-42009 in roundcube 1.6.6+dfsg-2ubuntu0.1: fix needs Ubuntu Pro, version 1.6.6+dfsg-2ubuntu0.1+esm1 (CISA: Test flaw CVE-2024-42009; https://ubuntu.com/security/CVE-2024-42009)",
			ExactFinding: "Ubuntu:24.04:LTS CVE-2024-42009 roundcube 1.6.6+dfsg-2ubuntu0.1; fix needs Ubuntu Pro: 1.6.6+dfsg-2ubuntu0.1+esm1; UBUNTU-CVE-2024-42009 modified 2026-10-26T07:05:12Z"},
		{ID: "CVE-2025-32463 sudo", Message: "CVE-2025-32463 in sudo 1.9.15p5-3ubuntu5.24.04.1: fixed version 1.9.15p5-3ubuntu5.24.04.2 waits to be installed (CISA: Test flaw CVE-2025-32463; https://ubuntu.com/security/CVE-2025-32463)",
			ExactFinding: "Ubuntu:24.04:LTS CVE-2025-32463 sudo 1.9.15p5-3ubuntu5.24.04.1; fixed version waits: 1.9.15p5-3ubuntu5.24.04.2; UBUNTU-CVE-2025-32463 modified 2026-10-26T07:05:12Z"},
	}
	if o.Status != "failed" || !reflect.DeepEqual(o.Findings, wantFindings) {
		t.Fatalf("%+v", o)
	}
	batches, fetched := standIn.asked()
	want := []string{}
	for _, source := range []string{"linux", "openssl", "roundcube", "sudo"} {
		want = append(want, main+" "+source, pro+" "+source)
	}
	slices.Sort(want)
	if got := queriesOf(batches); !reflect.DeepEqual(got, want) {
		t.Fatalf("queries: %q", got)
	}
	// A record that both ecosystems name is fetched once.
	slices.Sort(fetched)
	if want := []string{"UBUNTU-CVE-2024-42009", "UBUNTU-CVE-2025-32463", "UBUNTU-CVE-2026-5555", "UBUNTU-CVE-2026-6666"}; !reflect.DeepEqual(fetched, want) {
		t.Fatalf("fetched records: %q", fetched)
	}

	standIn.set(func() { standIn.cisaBody = cisaList("CVE-2026-5555") })
	if o := withinTimeLimit(t, 5*time.Second, check); o.Status != "ok" || o.Message != "no installed package has a flaw on CISA's list of known exploited vulnerabilities of 2026-10-26: MMWatchdog searched OSV.dev for 4 source packages of "+main+" and "+pro {
		t.Fatal(o)
	}

	// Flaws filed only under Ubuntu Pro do not show that OSV.dev knows the
	// release.
	standIn.set(func() { standIn.flaws = map[string]map[string][]string{pro: standIn.flaws[pro]} })
	if o := withinTimeLimit(t, 5*time.Second, check); o.Status != "unknown" || o.Message != "OSV.dev knows no flaw of any of the 4 source packages in "+main+", so it may not cover this release" {
		t.Fatal(o)
	}

	if err := os.WriteFile(release, []byte("ID=ubuntu\nVERSION_ID=\"25.10\"\nVERSION=\"25.10 (Questing Quokka)\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	standIn.set(func() {
		standIn.flaws = map[string]map[string][]string{"Ubuntu:25.10": {"sudo": {"UBUNTU-CVE-2025-32463"}}}
		standIn.records["UBUNTU-CVE-2025-32463"] = osvRecord("UBUNTU-CVE-2025-32463", osvEntry{"sudo", "Ubuntu:25.10", osvFixedIn("1.9.15p5-3ubuntu5.24.04.1")})
		standIn.cisaBody = cisaList("CVE-2025-32463")
	})
	standIn.asked()
	if o := withinTimeLimit(t, 5*time.Second, check); o.Status != "ok" || !strings.HasSuffix(o.Message, " of Ubuntu:25.10") {
		t.Fatal(o)
	}
	if batches, _ := standIn.asked(); !reflect.DeepEqual(queriesOf(batches), []string{"Ubuntu:25.10 linux", "Ubuntu:25.10 openssl", "Ubuntu:25.10 roundcube", "Ubuntu:25.10 sudo"}) {
		t.Fatal(queriesOf(batches))
	}
}

// OSV.dev takes at most 1,000 queries in one batch, so a node with more
// source packages asks in several requests.
func TestKnownExploitedVulnerabilitiesAskAtMostAThousandQueriesAtOnce(t *testing.T) {
	asThisAccount(t)
	nodeFiles(t, map[string]string{"etc/os-release": "ID=debian\nVERSION_ID=\"13\"\n", "proc/sys/kernel/osrelease": "not-packaged\n", "proc/version": "Linux version not-packaged (builder@example.com) #1 SMP\n"})
	packages, _, _ := clientProgram(t, "dpkg-query", "LC_ALL")
	many := strings.Builder{}
	for index := range 1001 {
		fmt.Fprintf(&many, "installed\tpackage-%04d\tpackage-%04d\t1.0\t1.0\t\n", index, index)
	}
	if err := os.WriteFile(packages, []byte(many.String()), 0600); err != nil {
		t.Fatal(err)
	}
	fakeProgram(t, "apt-cache", "true")
	standIn := newOSVStandIn(t)
	standIn.set(func() {
		standIn.cisaBody = cisaList("CVE-2026-1000")
		// The last package, which only a second request asks about.
		standIn.flaws = map[string]map[string][]string{"Debian:13": {"package-1000": {"DEBIAN-CVE-2026-1000"}}}
		standIn.records = map[string]string{"DEBIAN-CVE-2026-1000": osvRecord("DEBIAN-CVE-2026-1000", osvEntry{"package-1000", "Debian:13", osvFixedIn("2.0")})}
	})
	o := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.KnownExploitedVulnerabilitiesInInstalledPackages{FailWhen: "an_installed_package_is_affected"}})
	if o.Status != "failed" || len(o.Findings) != 1 || o.Findings[0].ID != "CVE-2026-1000 package-1000" {
		t.Fatal(o)
	}
	batches, _ := standIn.asked()
	if len(batches) != 2 || len(queriesOf(batches)) != 1001 || len(slices.Compact(queriesOf(batches))) != 1001 {
		t.Fatalf("%d batches with %d queries", len(batches), len(queriesOf(batches)))
	}
}

// A reboot_after_updates check reads /run/reboot-required on Debian and Ubuntu.
func TestRebootAfterUpdatesCheck(t *testing.T) {
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.RebootAfterUpdates{FailWhen: "the_node_must_reboot_to_use_installed_updates"}})
	}
	// On Debian and Ubuntu, a missing file says that no reboot is needed
	// only where a kernel hook writes it for a new kernel. A Debian node
	// without unattended-upgrades has hooks, but none that writes it.
	root := nodeFiles(t, map[string]string{"etc/os-release": "ID=debian\n", "run/": "", "etc/kernel/postinst.d/initramfs-tools": "#!/bin/sh\n", "etc/kernel/postinst.d/zz-update-grub": "#!/bin/sh\n"})
	os.Chmod(filepath.Join(root, "etc/kernel/postinst.d/initramfs-tools"), 0755)
	os.Chmod(filepath.Join(root, "etc/kernel/postinst.d/zz-update-grub"), 0755)
	const nothingWritesTheFile = "no program on this node writes /run/reboot-required when a new kernel is installed, so MMWatchdog cannot know whether the node needs a reboot: install the package unattended-upgrades"
	if o := observe(); o.Status != "unknown" || o.Message != nothingWritesTheFile {
		t.Fatal(o)
	}
	// Each package's hook that writes the file. The kernel's package runs
	// only an executable hook.
	for _, hook := range []string{"etc/kernel/postinst.d/unattended-upgrades", "etc/kernel/postinst.d/update-notifier", "etc/kernel/postinst.d/reboot-notifier", "usr/share/kernel/postinst.d/touch-reboot-required"} {
		path := filepath.Join(root, hook)
		os.MkdirAll(filepath.Dir(path), 0755)
		os.WriteFile(path, []byte("#!/bin/sh\ntouch /run/reboot-required\n"), 0644)
		if o := observe(); o.Status != "unknown" || o.Message != nothingWritesTheFile {
			t.Fatal(hook, o)
		}
		os.Chmod(path, 0755)
		if o := observe(); o.Status != "ok" || o.Message != "this node needs no reboot" {
			t.Fatal(hook, o)
		}
		os.Remove(path)
	}
	// A file that a package left says that the node needs a reboot, also
	// where no hook writes it for a new kernel, as dbus leaves it for a new
	// version of itself.
	os.WriteFile(filepath.Join(root, "run/reboot-required"), []byte("*** System restart required ***\n"), 0644)
	if o := observe(); o.Status != "failed" || o.Message != "this node needs a reboot (/run/reboot-required exists)" {
		t.Fatal(o)
	}
	os.WriteFile(filepath.Join(root, "run/reboot-required.pkgs"), []byte("linux-image-6.12.48+deb13-amd64\nlibc6\nlinux-image-6.12.48+deb13-amd64\n"), 0644)
	if o := observe(); o.Status != "failed" || o.Message != "this node needs a reboot for 2 packages: libc6, linux-image-6.12.48+deb13-amd64" {
		t.Fatal(o)
	}
}

// An apt_security_updates check reads what the node's package manager
// already knows: which packages an upgrade would take from a security
// archive.
func TestAptSecurityUpdatesCheck(t *testing.T) {
	ranAs := asThisAccount(t)
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.AptSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}})
	}
	// Debian and Ubuntu: apt simulates the upgrade.
	root := nodeFiles(t, map[string]string{"etc/os-release": "ID=debian\n", "var/lib/apt/lists/partial/": "", "var/lib/apt/lists/repository_Packages": "Package: test\n", "var/lib/apt/periodic/update-success-stamp": ""})
	answer, exitCode, asked := clientProgram(t, "apt-get", "LC_ALL")
	const simulation = `Reading package lists...
Building dependency tree...
Reading state information...
Calculating upgrade...
The following packages will be upgraded:
  curl libssl3 linux-image-amd64 openssl tzdata
Inst libssl3 [3.0.11-1~deb12u1] (3.0.13-1~deb12u1 Debian-Security:12/stable-security [amd64]) [openssl:amd64 ]
Inst openssl [3.0.11-1~deb12u1] (3.0.13-1~deb12u1 Debian-Security:12/stable-security [amd64])
Inst tzdata [2024a-0+deb12u1] (2024b-0+deb12u1 Debian:12.8/stable-updates [all])
Inst curl [7.88.1-10+deb12u5] (7.88.1-10+deb12u8 Debian:12.8/stable, Debian-Security:12/stable-security [amd64])
Inst linux-image-6.1.0-28-amd64 (6.1.119-1 Debian-Security:12/stable-security [amd64])
Conf libssl3 (3.0.13-1~deb12u1 Debian-Security:12/stable-security [amd64])
`
	os.WriteFile(answer, []byte(simulation), 0644)
	o := observe()
	if o.Status != "failed" || o.Message != "4 security updates wait to be installed: curl, libssl3, linux-image-6.1.0-28-amd64, openssl" {
		t.Fatal(o)
	}
	// apt only simulates, as the unprivileged account, which can change
	// nothing.
	if data, _ := os.ReadFile(asked); string(data) != "--simulate\n--quiet\ndist-upgrade\nLC_ALL=C\n" || !reflect.DeepEqual(ranAs(), []string{"mmwatchdog"}) {
		t.Fatalf("%q", data)
	}
	// Ubuntu names its security archive in another way.
	os.WriteFile(answer, []byte("Inst libc6 [2.39-0ubuntu8.3] (2.39-0ubuntu8.4 Ubuntu:24.04/noble-security [amd64])\nInst vim [2:9.1.0016-1ubuntu7.5] (2:9.1.0016-1ubuntu7.6 Ubuntu:24.04/noble-updates [amd64])\n"), 0644)
	if o := observe(); o.Status != "failed" || o.Message != "1 security update waits to be installed: libc6" {
		t.Fatal(o)
	}
	os.WriteFile(answer, []byte("Reading package lists...\nInst tzdata [2024a] (2024b Debian:12.8/stable-updates [all])\n0 upgraded, 0 newly installed.\n"), 0644)
	if o := observe(); o.Status != "ok" || o.Message != "no security update waits to be installed" {
		t.Fatal(o)
	}
	// Lists of available packages that are too old say nothing about the
	// updates of the last days.
	old := time.Now().Add(-9 * 24 * time.Hour)
	for _, path := range []string{"var/lib/apt/lists/repository_Packages", "var/lib/apt/periodic/update-success-stamp"} {
		os.Chtimes(filepath.Join(root, path), old, old)
	}
	if o := observe(); o.Status != "unknown" || o.Message != "the stamp /var/lib/apt/periodic/update-success-stamp, which apt writes after a successful refresh, is 9 days old, so MMWatchdog cannot know which updates wait: have apt write this stamp after each successful refresh; the module page apt_security_updates, section \"Set up\", shows how (mmwatchdog describe-module apt_security_updates)" {
		t.Fatal(o)
	}
	// A recent failed attempt touches directories and update-stamp,
	// without making stale downloaded lists fresh.
	os.MkdirAll(filepath.Join(root, "var/lib/apt/periodic"), 0755)
	os.WriteFile(filepath.Join(root, "var/lib/apt/periodic/update-stamp"), nil, 0644)
	if o := observe(); o.Status != "unknown" {
		t.Fatal(o)
	}
	// The stamp of the periodic refresh counts too.
	os.MkdirAll(filepath.Join(root, "var/lib/apt/periodic"), 0755)
	os.WriteFile(filepath.Join(root, "var/lib/apt/periodic/update-success-stamp"), nil, 0644)
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	os.WriteFile(exitCode, []byte("100"), 0644)
	os.WriteFile(answer+".stderr", []byte("E: The package lists or status file could not be parsed or opened.\n"), 0644)
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot ask apt-get which updates wait: E: The package lists or status file could not be parsed or opened." {
		t.Fatal(o)
	}
	nodeFiles(t, map[string]string{"etc/os-release": "ID=debian\n", "var/lib/apt/lists/partial/": "", "var/lib/apt/periodic/update-success-stamp": ""})
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "apt has no lists of available packages on this node, so MMWatchdog cannot know which updates wait: ") {
		t.Fatal(o)
	}

	loadChecks(t, map[string]string{
		"{apt_security_updates: {fail_when: a_security_update_waits_to_be_installed}, alert_severity: warn}":                                          "",
		"{apt_security_updates: {fail_when: another_rule}, alert_severity: warn}":                                                                     "check_groups.c.apt_security_updates.fail_when: expected one of a_security_update_waits_to_be_installed",
		"{reboot_after_updates: {fail_when: the_node_must_reboot_to_use_installed_updates}, alert_severity: info}":                                    "",
		"{distribution_release_security_support: {security_support: regular, fail_when_security_support_ends_within: 180d}, alert_severity: warn}":    "",
		"{distribution_release_security_support: {fail_when_security_support_ends_within: 180d}, alert_severity: warn}":                               "distribution_release_security_support.security_support: is required",
		"{distribution_release_security_support: {security_support: other, fail_when_security_support_ends_within: 180d}, alert_severity: warn}":      "expected one of regular, debian_lts, ubuntu_pro",
		"{distribution_release_security_support: {security_support: debian_lts, fail_when_security_support_ends_within: 180d}, alert_severity: warn}": "",
		"{distribution_release_security_support: {security_support: regular}, alert_severity: warn}":                                                  "is required",
		"{distribution_release_security_support: {security_support: regular, fail_when_security_support_ends_within: soon}, alert_severity: warn}":    "expected a duration such as 30s, 5m, 26h, or 7d",
		"{reboot_after_updates: {fail_when: another_rule}, alert_severity: info}":                                                                     "check_groups.c.reboot_after_updates.fail_when: expected one of the_node_must_reboot_to_use_installed_updates",
		"{reboot_after_updates: {fail_when: the_node_must_reboot_to_use_installed_updates}}":                                                          "check_groups.c.alert_severity: required: info, warn, or sos",
	})
}

func TestUbuntuProSecurityFixesCheck(t *testing.T) {
	ranAs := asThisAccount(t)
	root := nodeFiles(t, map[string]string{"etc/os-release": "ID=ubuntu\nVERSION_ID=\"24.04\"\nPRETTY_NAME=Ubuntu 24.04 LTS\n", "var/lib/apt/lists/repository_Packages": "Package: test\n", "var/lib/apt/periodic/update-success-stamp": "", "var/lib/ubuntu-advantage/apt-esm/var/lib/apt/lists/esm_Packages": "Package: test\n"})
	answer, exitCode, asked := clientProgram(t, "pro", "LC_ALL")
	inventory, inventoryExit, inventoryAsked := clientProgram(t, "dpkg-query", "LC_ALL")
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Two binary packages of one source package at different versions: the
	// exact finding names the lower one.
	write(inventory, "installed\tpython3-wheel\tpython-wheel\t0.42.0-1\ninstalled\tpython-wheel-common\tpython-wheel\t0.41.0-2\ninstalled\ttillitis-tkey-udev\ttillitis-tkey\t1.0.0+ds-6\nconfig-files\tremoved\twrong-source\t1\n")
	const wheel = `{"package":"python3-wheel","version":"0.42.0-2ubuntu0.1~esm1","service_name":"esm-apps","status":"pending_attach"},{"package":"python-wheel-common","version":"0.42.0-2ubuntu0.1~esm1","service_name":"esm-apps","status":"pending_attach"}`
	const tkey = `{"package":"tillitis-tkey-udev","version":"1.0.0+ds-6ubuntu0.1~esm1","service_name":"esm-apps","status":"pending_attach"}`
	status := func(packages string, attached bool) {
		write(answer, fmt.Sprintf(`{"packages":[%s],"summary":{"ua":{"attached":%t}}}`, packages, attached))
	}
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.UbuntuProSecurityFixes{FailWhen: "the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers"}})
	}
	status(tkey+","+wheel+`,{"package":"zlib1g","version":"1.3","service_name":"standard-security","status":"upgrade_available"},{"package":"available","version":"1","service_name":"esm-apps","status":"upgrade_available"}`, false)
	o := observe()
	want := []checksandalerts.Finding{
		{ID: "python-wheel", Message: "python-wheel has a security fix that this node misses because only Ubuntu Pro's ESM Apps delivers it: version 0.42.0-2ubuntu0.1~esm1 of the installed packages python-wheel-common, python3-wheel",
			ExactFinding: "ubuntu 24.04 python-wheel 0.41.0-2; esm-apps fixed version: 0.42.0-2ubuntu0.1~esm1"},
		{ID: "tillitis-tkey", Message: "tillitis-tkey has a security fix that this node misses because only Ubuntu Pro's ESM Apps delivers it: version 1.0.0+ds-6ubuntu0.1~esm1 of the installed package tillitis-tkey-udev",
			ExactFinding: "ubuntu 24.04 tillitis-tkey 1.0.0+ds-6; esm-apps fixed version: 1.0.0+ds-6ubuntu0.1~esm1"},
	}
	if o.Status != "failed" || o.Message != "2 source packages have a security fix that this node misses because only Ubuntu Pro delivers it: python-wheel, tillitis-tkey" || !reflect.DeepEqual(o.Findings, want) {
		t.Fatal(o)
	}
	if !(&checksandalerts.Check{Inputs: new(checksandalerts.UbuntuProSecurityFixes)}).Module().OneIncidentForEachFinding || o.FindingIsGoneMessage == nil || o.FindingIsGoneMessage("python-wheel") != "this node no longer misses a security fix of python-wheel that only Ubuntu Pro delivers" {
		t.Fatal("missing finding lifecycle", o)
	}
	if data, _ := os.ReadFile(asked); string(data) != "security-status\n--format\njson\nLC_ALL=C\n" {
		t.Fatalf("pro arguments: %q", data)
	}
	if data, _ := os.ReadFile(inventoryAsked); string(data) != "--show\n--showformat=${db:Status-Status}\t${Package}\t${source:Package}\t${source:Version}\n\nLC_ALL=C\n" {
		t.Fatalf("dpkg-query arguments: %q", data)
	}
	if !reflect.DeepEqual(ranAs(), []string{"mmwatchdog", "mmwatchdog"}) {
		t.Fatal("wrong command accounts")
	}
	status(strings.ReplaceAll(wheel, "esm-apps", "esm-infra"), true)
	o = observe()
	if o.Status != "failed" || o.Message != "1 source package has a security fix that this node misses because only Ubuntu Pro delivers it: python-wheel" || o.Findings[0].Message != strings.ReplaceAll(want[0].Message, "ESM Apps", "ESM Infra") {
		t.Fatal(o)
	}
	status(`{"package":"python3-wheel","version":"1.1","service_name":"esm-apps","status":"pending_enable"},{"package":"python-wheel-common","version":"1.0","service_name":"esm-apps","status":"pending_enable"}`, true)
	o = observe()
	if o.Status != "failed" || o.Findings[0].Message != "python-wheel has a security fix that this node misses because only Ubuntu Pro's ESM Apps delivers it: the installed packages python-wheel-common 1.0, python3-wheel 1.1" {
		t.Fatal(o)
	}
	status(wheel+","+strings.ReplaceAll(tkey, "esm-apps", "esm-infra"), false)
	o = observe()
	if o.Status != "failed" || len(o.Findings) != 2 || o.Findings[1].Message != strings.ReplaceAll(want[1].Message, "ESM Apps", "ESM Infra") {
		t.Fatal(o)
	}
	status(`{"package":"python3-wheel","version":"1.1","service_name":"esm-apps","status":"pending_enable"},{"package":"python-wheel-common","version":"1.0","service_name":"esm-infra","status":"pending_enable"}`, true)
	o = observe()
	if o.Status != "failed" || len(o.Findings) != 1 || o.Findings[0].ID != "python-wheel" || o.Findings[0].Message != "python-wheel has a security fix that this node misses because only Ubuntu Pro's ESM Apps delivers it: version 1.1 of the installed package python3-wheel; python-wheel has a security fix that this node misses because only Ubuntu Pro's ESM Infra delivers it: version 1.0 of the installed package python-wheel-common" ||
		o.Findings[0].ExactFinding != "ubuntu 24.04 python-wheel 0.41.0-2; esm-apps fixed version: 1.1; esm-infra fixed version: 1.0" {
		t.Fatal(o)
	}

	status(`{"package":"unlisted","version":"1","service_name":"esm-apps","status":"pending_attach"},{"package":"removed","version":"1","service_name":"esm-apps","status":"pending_attach"}`, false)
	o = observe()
	if o.Status != "failed" || len(o.Findings) != 2 || o.Findings[0].ID != "removed" || o.Findings[1].ID != "unlisted" || o.Findings[1].ExactFinding != "ubuntu 24.04 unlisted installed version unknown; esm-apps fixed version: 1" {
		t.Fatal(o)
	}
	status(`{"package":"standard","version":"1","service_name":"standard-security","status":"pending_attach"},{"package":"available","version":"1","service_name":"esm-infra","status":"upgrade_available"}`, false)
	if o := observe(); o.Status != "ok" || o.Message != "this node misses no security fix that only Ubuntu Pro delivers" || len(o.Findings) != 0 {
		t.Fatal(o)
	}
	// An empty file is not evidence of any ESM package data.
	write(filepath.Join(root, "var/lib/ubuntu-advantage/apt-esm/var/lib/apt/lists/esm_Packages"), "")
	if o := observe(); o.Status != "unknown" || o.Message != "the Ubuntu Pro client has no lists of ESM fixes in /var/lib/ubuntu-advantage/apt-esm, so MMWatchdog cannot know which fixes only Ubuntu Pro delivers: run apt-get update as root, which starts the Ubuntu Pro client's esm-cache.service" {
		t.Fatal(o)
	}
	status("", true)
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	for _, json := range []string{"not JSON", `{}`, `{"packages":[],"summary":{"ua":{}}}`, `{"packages":null,"summary":{"ua":{"attached":false}}}`, `{"packages":{},"summary":{"ua":{"attached":false}}}`, `{"packages":[],"summary":{"ua":{"attached":"false"}}}`, `{"packages":[],"summary":{"ua":{"attached":null}}}`, `{"packages":[{"service_name":"esm-apps","status":"pending_attach"}],"summary":{"ua":{"attached":true}}}`} {
		write(answer, json)
		if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot read pro security-status JSON:") {
			t.Fatal(json, o)
		}
	}
	status("", true)
	write(exitCode, "1")
	write(answer+".stderr", "pro failed\n")
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot ask pro which updates wait: pro failed" {
		t.Fatal(o)
	}
	write(exitCode, "0")
	write(inventoryExit, "1")
	write(inventory+".stderr", "dpkg-query failed\n")
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot read installed packages with dpkg-query: dpkg-query failed" {
		t.Fatal(o)
	}
	write(inventoryExit, "0")
	stamp := filepath.Join(root, "var/lib/apt/periodic/update-success-stamp")
	old := time.Now().Add(-9 * 24 * time.Hour)
	if err := os.Chtimes(stamp, old, old); err != nil {
		t.Fatal(err)
	}
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "the stamp /var/lib/apt/periodic/update-success-stamp, which apt writes after a successful refresh, is 9 days old") {
		t.Fatal(o)
	}
	if err := os.Rename(stamp, stamp+".saved"); err != nil {
		t.Fatal(err)
	}
	write(filepath.Join(root, "var/lib/apt/periodic/update-stamp"), "")
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "apt has no usable successful-refresh stamp") {
		t.Fatal(o)
	}
	write(stamp, "")
	write(filepath.Join(root, "var/lib/apt/lists/repository_Packages"), "")
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "apt has no lists of available packages") {
		t.Fatal(o)
	}
	write(filepath.Join(root, "etc/os-release"), "ID=debian\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n")
	if o := observe(); o.Status != "unknown" || o.Message != "Ubuntu Pro exists only on Ubuntu, and this node runs Debian GNU/Linux 13 (trixie)" {
		t.Fatal(o)
	}
	write(filepath.Join(root, "etc/os-release"), "ID=other\nPRETTY_NAME=Other Linux\n")
	if o := observe(); o.Status != "unknown" || o.Message != "Ubuntu Pro exists only on Ubuntu, and this node runs Other Linux" {
		t.Fatal(o)
	}
	write(filepath.Join(root, "etc/os-release"), "ID=derivative\nID_LIKE=\"debian ubuntu\"\n")
	write(filepath.Join(root, "var/lib/apt/lists/repository_Packages"), "Package: test\n")
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	// No pro command is available in the command lookup path.
	testhelpers.SetProgramEnvironment(t, "PATH", t.TempDir())
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot run pro, the command of Ubuntu's package ubuntu-pro-client:") || !strings.HasSuffix(o.Message, ": install ubuntu-pro-client") {
		t.Fatal(o)
	}
}

// An mdadm_array check reads the state of one software RAID array from the
// kernel's files, and fails while the array misses a device and while the
// node has not put the array together.
func TestMdadmArrayCheck(t *testing.T) {
	nodeFiles(t, map[string]string{"dev/md0": "", "dev/md1": ""})
	md := "sys/block/md0/md/"
	root := testhelpers.KernelFiles(t, map[string]string{md + "array_state": "clean\n", md + "degraded": "0\n", md + "raid_disks": "2\n", md + "level": "raid1\n", md + "sync_action": "idle\n", md + "sync_completed": "none\n",
		md + "dev-sda1/state": "in_sync\n", md + "dev-sdb1/state": "in_sync\n", md + "rd0/": ""})
	set := func(file, content string) { os.WriteFile(filepath.Join(root, md+file), []byte(content+"\n"), 0644) }
	observe := func(path string) checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: new(checksandalerts.MdadmArray(path))})
	}
	if o := observe("/dev/md0"); o.Status != "ok" || o.Message != "/dev/md0 (raid1) has all expected devices: 2 of 2 devices work" {
		t.Fatal(o)
	}
	// The kernel checks the array from time to time, which is no failure.
	set("sync_action", "check")
	set("sync_completed", "1048576 / 4194304")
	if o := observe("/dev/md0"); o.Status != "ok" || o.Message != "/dev/md0 (raid1) has all expected devices: 2 of 2 devices work; the kernel runs a check of the array, 25 percent done" {
		t.Fatal(o)
	}
	// A device fails.
	set("sync_action", "idle")
	set("sync_completed", "none")
	set("degraded", "1")
	set("dev-sdb1/state", "faulty")
	if o := observe("/dev/md0"); o.Status != "failed" || o.Message != "/dev/md0 (raid1) is degraded: 1 of 2 devices work; the kernel reports as faulty: sdb1" {
		t.Fatal(o)
	}
	// A new device is being rebuilt: the array is degraded until it is done.
	set("dev-sdb1/state", "spare")
	set("sync_action", "recover")
	set("sync_completed", "2097152 / 4194304")
	if o := observe("/dev/md0"); o.Status != "failed" || o.Message != "/dev/md0 (raid1) is degraded: 1 of 2 devices work; the array is being rebuilt, 50 percent done" {
		t.Fatal(o)
	}
	set("array_state", "inactive")
	if o := observe("/dev/md0"); o.Status != "failed" || o.Message != "/dev/md0 does not work: the kernel reports the array as inactive" {
		t.Fatal(o)
	}
	set("array_state", "clean")
	for _, degraded := range []string{"", "invalid", "-1"} {
		set("degraded", degraded)
		if o := observe("/dev/md0"); o.Status != "unknown" || !strings.Contains(o.Message, "degraded device count") || !strings.Contains(o.Message, "/dev/md0") {
			t.Fatal(o)
		}
	}
	if o := observe("/dev/missing/md0"); o.Status != "failed" || !strings.Contains(o.Message, "cannot resolve the device path /dev/missing/md0") {
		t.Fatal(o)
	}
	// An array that the node has not put together must not look healthy.
	if o := observe("/dev/md1"); o.Status != "failed" || o.Message != "/dev/md1 is not an assembled software RAID array on the node" {
		t.Fatal(o)
	}
	testhelpers.KernelFiles(t, map[string]string{md + "array_state": "clean", md + "degraded/": ""})
	if o := observe("/dev/md0"); o.Status != "unknown" || !strings.Contains(o.Message, "degraded device count") {
		t.Fatal(o)
	}
	// An array without redundancy has no degraded count; a lost device
	// makes the kernel report the array as broken.
	root = testhelpers.KernelFiles(t, map[string]string{md + "array_state": "clean\n", md + "raid_disks": "2\n", md + "level": "raid1\n", md + "dev-sda1/state": "in_sync\n", md + "dev-sdb1/state": "in_sync\n"})
	if o := observe("/dev/md0"); o.Status != "unknown" || !strings.Contains(o.Message, "degraded device count") {
		t.Fatal("a raid1 array without a degraded count looks healthy:", o)
	}
	set("level", "raid0")
	if o := observe("/dev/md0"); o.Status != "ok" || o.Message != "/dev/md0 (raid0) has all expected devices: 2 of 2 devices work" {
		t.Fatal(o)
	}
	set("array_state", "broken")
	if o := observe("/dev/md0"); o.Status != "failed" || o.Message != "/dev/md0 does not work: the kernel reports the array as broken" {
		t.Fatal(o)
	}
	loadChecks(t, map[string]string{
		"{mdadm_array: /dev/md0, alert_severity: sos}":     "",
		"{mdadm_array: /dev/md/data, alert_severity: sos}": "",
		"{mdadm_array: md0, alert_severity: sos}":          "check_groups.c.mdadm_array: must be the path of a software RAID array, such as /dev/md0",
		"{mdadm_array: /dev/md0}":                          "check_groups.c.alert_severity: required: info, warn, or sos",
	})
}

// A node_temperature check measures the highest reading among the sensors
// of one name.
func TestNodeTemperatureCheck(t *testing.T) {
	hwmon := "sys/class/hwmon/"
	root := testhelpers.KernelFiles(t, map[string]string{
		hwmon + "hwmon0/name": "coretemp\n", hwmon + "hwmon0/temp1_input": "61000\n", hwmon + "hwmon0/temp1_label": "Package id 0\n", hwmon + "hwmon0/temp2_input": "58500\n", hwmon + "hwmon0/temp2_label": "Core 0\n",
		hwmon + "hwmon0/temp1_max": "100000\n", hwmon + "hwmon0/fan1_input": "1200\n",
		hwmon + "hwmon1/name": "nvme\n", hwmon + "hwmon1/temp1_input": "38850\n", hwmon + "hwmon1/temp1_label": "Composite\n",
		hwmon + "hwmon2/name": "nvme\n", hwmon + "hwmon2/temp1_input": "44850\n",
		hwmon + "hwmon3/name": "outside\n", hwmon + "hwmon3/temp1_input": "-4500\n",
	})
	observe := func(sensor string, above float64) checksandalerts.Observation {
		check := checksandalerts.Check{Inputs: new(checksandalerts.NodeTemperature(sensor))}
		if above != 0 {
			check.FailWhenMeasurementAbove, check.AlertSeverity = map[string]float64{"highest_temperature_celsius": above}, "warn"
		}
		return withinTimeLimit(t, 5*time.Second, check)
	}
	o := observe("coretemp", 85)
	if o.Status != "ok" || o.Message != "highest_temperature_celsius is 61 (fails above 85); the hottest of 2 sensors of coretemp is Package id 0" || o.Measurements["highest_temperature_celsius"] != (checksandalerts.Measurement{Value: 61, Unit: "degrees Celsius"}) {
		t.Fatal(o)
	}
	if o := observe("coretemp", 60); o.Status != "failed" || !strings.HasPrefix(o.Message, "highest_temperature_celsius is 61 (fails above 60); ") {
		t.Fatal(o)
	}
	// Two drives have sensors of one name: the hotter one counts, and a
	// sensor without a label has the name of its file.
	if o := observe("nvme", 0); o.Status != "ok" || o.Message != "highest_temperature_celsius is 44.85; the hottest of 2 sensors of nvme is temp1" {
		t.Fatal(o)
	}
	// A temperature below zero is a valid reading.
	if o := observe("outside", 0); o.Status != "ok" || o.Measurements["highest_temperature_celsius"].Value != -4.5 {
		t.Fatal(o)
	}
	if o := observe("outside", -10); o.Status != "failed" || o.Measurements["highest_temperature_celsius"].Value != -4.5 {
		t.Fatal(o)
	}
	if o := observe("k10temp", 85); o.Status != "unknown" || o.Message != "this machine has no temperature sensor named k10temp; the kernel knows these kinds of sensor: coretemp, nvme, outside" {
		t.Fatal(o)
	}
	badSensor := filepath.Join(root, hwmon+"hwmon0/temp3_input")
	if err := os.WriteFile(badSensor, []byte("not a temperature"), 0644); err != nil {
		t.Fatal(err)
	}
	if o := observe("coretemp", 85); o.Status != "unknown" || !strings.Contains(o.Message, badSensor) || len(o.Measurements) != 0 {
		t.Fatal(o)
	}
	if o := observe("coretemp", 60); o.Status != "failed" || !strings.Contains(o.Message, badSensor) {
		t.Fatal(o)
	}
	unreadableSensor := filepath.Join(root, hwmon+"hwmon0/temp4_input")
	if err := os.Symlink(filepath.Join(root, "missing-sensor"), unreadableSensor); err != nil {
		t.Fatal(err)
	}
	if o := observe("coretemp", 85); o.Status != "unknown" || !strings.Contains(o.Message, unreadableSensor) {
		t.Fatal(o)
	}
	// A virtual machine often has no sensor at all.
	testhelpers.KernelFiles(t, map[string]string{"sys/class/": ""})
	if o := observe("coretemp", 85); o.Status != "unknown" || o.Message != "this machine has no temperature sensor named coretemp; it has no temperature sensor that the kernel knows" {
		t.Fatal(o)
	}
	loadChecks(t, map[string]string{
		"{node_temperature: coretemp, fail_when_measurement_above: {highest_temperature_celsius: 85}, alert_severity: warn}": "",
		"{node_temperature: outside, fail_when_measurement_below: {highest_temperature_celsius: -5}, alert_severity: warn}":  "",
		"{node_temperature: outside, fail_when_measurement_above: {highest_temperature_celsius: -10}, alert_severity: warn}": "",
		"{only_record_readings: true, node_temperature: nvme}":                                                               "",
		"{node_temperature: coretemp, alert_severity: warn}":                                                                 "a node_temperature check needs a limit to alert",
		"{only_record_readings: true, node_temperature: 'core temp'}":                                                        "check_groups.c.node_temperature: must be the name of a kind of temperature sensor",
		"{node_temperature: coretemp, fail_when_measurement_above: {degrees: 85}, alert_severity: warn}":                     "check_groups.c.fail_when_measurement_above.degrees: this check has no such measurement; it measures highest_temperature_celsius",
	})
}

func TestSecurityUpdateRefreshStampTakesPrecedenceOverFreshLists(t *testing.T) {
	asThisAccount(t)
	for _, manager := range []struct{ distribution, stamp, list string }{
		{"debian", "var/lib/apt/periodic/update-success-stamp", "var/lib/apt/lists/unrelated_Packages"},
	} {
		root := nodeFiles(t, map[string]string{"etc/os-release": "ID=" + manager.distribution + "\n", manager.stamp: "", manager.list: "fresh package metadata"})
		old := time.Now().Add(-9 * 24 * time.Hour)
		if err := os.Chtimes(filepath.Join(root, manager.stamp), old, old); err != nil {
			t.Fatal(err)
		}
		o := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.AptSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}})
		if o.Status != "unknown" || !strings.Contains(o.Message, "the stamp /"+manager.stamp) || !strings.Contains(o.Message, "is 9 days old") {
			t.Fatal(o)
		}
	}
}

func TestSecurityUpdateCheckRequiresSuccessfulRefreshStamp(t *testing.T) {
	asThisAccount(t)
	for _, manager := range []struct{ distribution, program, stamp, list, hint string }{
		{"debian", "apt-get", "/var/lib/apt/periodic/update-success-stamp", "var/lib/apt/lists/repository_Packages", "mmwatchdog describe-module apt_security_updates"},
	} {
		t.Run(manager.distribution, func(t *testing.T) {
			nodeFiles(t, map[string]string{"etc/os-release": "ID=" + manager.distribution + "\n", manager.list: "fresh metadata from only one repository"})
			answer, _, asked := clientProgram(t, manager.program, "LC_ALL")
			if err := os.WriteFile(answer, nil, 0644); err != nil {
				t.Fatal(err)
			}
			observation := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.AptSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}})
			if observation.Status != "unknown" || !strings.Contains(observation.Message, manager.stamp) || !strings.Contains(observation.Message, manager.hint) {
				t.Fatal(observation)
			}
			if _, err := os.Stat(asked); !os.IsNotExist(err) {
				t.Fatal("queried package manager without evidence of a successful refresh", err)
			}
		})
	}
}
