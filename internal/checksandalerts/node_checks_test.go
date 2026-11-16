package checksandalerts_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// The tests of the checks of Unix sockets, ping, the clock, out-of-memory
// kills, files, and log lines, and of the system log destination. The rules
// are in docs/changing-mmwatchdog/design/checks.md under the heading of each check, and in
// docs/changing-mmwatchdog/design/alerts.md under "System log".

// scheduledRuns runs one check again and again as the daemon does: each run
// gets the memory of the runs before, and the state that the run before
// saved.
type scheduledRuns struct {
	check  checksandalerts.Check
	timing checksandalerts.Timing
	memory *checksandalerts.CheckMemory
	stored json.RawMessage
}

func (s *scheduledRuns) run(ctx context.Context) checksandalerts.Observation {
	if s.memory == nil {
		s.memory = &checksandalerts.CheckMemory{}
	}
	run := &checksandalerts.CheckRun{Check: s.check, Timing: s.timing, Memory: s.memory, State: &checksandalerts.ModuleState{Stored: s.stored}}
	o := checksandalerts.RunCheck(ctx, run)
	if run.State.Saved != nil {
		s.stored = run.State.Saved
	}
	return o
}

// fakeProgram puts a shell script on PATH under the name of a program.
func fakeProgram(t *testing.T, name, script string) {
	t.Helper()
	directory := t.TempDir()
	testhelpers.WriteStandInProgram(t, filepath.Join(directory, name), "#!/bin/sh\n"+script+"\n")
	testhelpers.PutProgramsFirst(t, directory)
}

// loadChecks loads each configuration of one check group named "c" and
// compares the error with the one that is wanted, or with none for "".
func loadChecks(t *testing.T, cases map[string]string) {
	t.Helper()
	for text, want := range cases {
		_, err := config.Load(configFileWithoutCheckSeverities(t, "check_groups: {c: "+text+"}"))
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s:\n got %v\nwant %q", text, err, want)
		}
	}
}

func TestNodeChecksRequireTheirFailureRule(t *testing.T) {
	t.Parallel()
	for _, module := range []struct{ name, failWhen, meaning, otherInputs string }{
		{"apt_security_updates", "a_security_update_waits_to_be_installed", "The check fails while a security update waits to be installed", ""},
		{"ubuntu_pro_security_fixes", "the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers", "The check fails while the node misses a security fix that only Ubuntu Pro delivers", ""},
		{"reboot_after_updates", "the_node_must_reboot_to_use_installed_updates", "The check fails while the node must reboot to use installed updates", ""},
		{"out_of_memory_kills", "the_kernel_kills_a_process_for_lack_of_memory", "The check fails when the kernel kills a process for lack of memory", ""},
		{"node_clock", "the_clock_is_not_synchronized", "The check fails while the clock is not synchronized", ""},
		{"every_systemd_unit", "a_systemd_unit_has_failed", "The check fails while a systemd unit has failed", "systemd_units_to_ignore: []"},
		{"systemd_unit_state", "the_unit_is_not_active", "The check fails while systemd reports the unit as neither active nor reloading", "systemd_unit: myapp.service"},
	} {
		t.Run(module.name, func(t *testing.T) {
			for _, test := range []struct{ name, input, message string }{
				{"allowed", "fail_when: " + module.failWhen, ""},
				{"missing", "", "is required: " + module.meaning + "; write " + module.failWhen + ", the only value."},
				{"wrong", "fail_when: another_rule", "expected one of " + module.failWhen},
				{"empty", "fail_when: ''", "expected one of " + module.failWhen},
				{"boolean", "fail_when: true", "expected a string, found true"},
			} {
				t.Run(test.name, func(t *testing.T) {
					inputs := strings.Trim(strings.Join([]string{test.input, module.otherInputs}, ", "), ", ")
					_, err := config.Load(configFileWithoutCheckSeverities(t, "check_groups: {c: {"+module.name+": {"+inputs+"}, alert_severity: warn}}"))
					if test.message == "" {
						if err != nil {
							t.Fatal(err)
						}
					} else if want := "check_groups.c." + module.name + ".fail_when: " + test.message; err == nil || !strings.HasSuffix(err.Error(), want) {
						t.Fatalf("got %v; want an error ending with %q", err, want)
					}
				})
			}
		})
	}
}

// A unix_socket check is healthy when the connection opens. A missing
// socket file and a socket that nobody listens on are failed.
func TestUnixSocketCheck(t *testing.T) {
	t.Parallel()
	// The path of a Unix socket has at most about 100 bytes.
	directory, err := os.MkdirTemp("/tmp", "mmwatchdog")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "app.sock")
	observe := func() checksandalerts.Observation {
		return checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: new(checksandalerts.UnixSocket(path))})
	}
	if o := observe(); o.Status != "failed" || o.Message != path+" does not exist" {
		t.Fatal(o)
	}
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	if o := observe(); o.Status != "ok" || o.Message != "Unix socket connection accepted" {
		t.Fatal(o)
	}
	// A program that ended without removing its socket file.
	listener.Close()
	leaveUnixSocketFile(t, path)
	if o := observe(); o.Status != "failed" || o.Message != "no program accepts connections on the Unix socket "+path {
		t.Fatal(o)
	}
	loadChecks(t, map[string]string{
		"{unix_socket: /run/php/php-fpm.sock, alert_severity: sos}":                                               "",
		"{unix_socket: php-fpm.sock, alert_severity: sos}":                                                        "check_groups.c.unix_socket: must be the absolute path of a Unix socket, such as /run/php/php-fpm.sock",
		"{unix_socket: /run/php/../x.sock, alert_severity: sos}":                                                  "check_groups.c.unix_socket: must be the absolute path of a Unix socket",
		"{unix_socket: /run/x.sock}":                                                                              "check_groups.c.alert_severity: required: info, warn, or sos",
		"{unix_socket: /run/x.sock, fail_when_measurement_above: {connections: 1}, alert_severity: warn}":         "check_groups.c.fail_when_measurement_above: the module unix_socket reports no measurement, so its check takes no limit",
		"{unix_socket: {path: /run/x.sock}, alert_severity: sos}":                                                 "expected a string",
		"{node_clock: {fail_when: the_clock_is_not_synchronized}, alert_severity: warn}":                          "",
		"{node_clock: {fail_when: another_rule}, alert_severity: warn}":                                           "check_groups.c.node_clock.fail_when: expected one of the_clock_is_not_synchronized",
		"{node_clock: true, alert_severity: warn}":                                                                "expected a mapping",
		"{node_clock: {}, alert_severity: warn}":                                                                  "check_groups.c.node_clock.fail_when: is required: The check fails while the clock is not synchronized; write the_clock_is_not_synchronized, the only value.",
		"{node_clock: {fail_when: the_clock_is_not_synchronized}}":                                                "check_groups.c.alert_severity: required: info, warn, or sos",
		"{out_of_memory_kills: {fail_when: the_kernel_kills_a_process_for_lack_of_memory}, alert_severity: warn}": "",
		"{out_of_memory_kills: {fail_when: another_rule}, alert_severity: warn}":                                  "check_groups.c.out_of_memory_kills.fail_when: expected one of the_kernel_kills_a_process_for_lack_of_memory",
		"{out_of_memory_kills: {fail_when: the_kernel_kills_a_process_for_lack_of_memory}, fail_when_measurement_above: {kills: 1}, alert_severity: warn}": "the module out_of_memory_kills reports no measurement, so its check takes no limit",
		"{ping: db-1.internal.example.com, alert_severity: warn}":                                         "",
		"{ping: 10.0.0.5, alert_severity: warn}":                                                          "",
		"{ping: 'fe80::1', alert_severity: warn}":                                                         "",
		"{ping: db-1, fail_when_measurement_above: {round_trip_milliseconds: 100}, alert_severity: warn}": "",
		"{ping: db-1, fail_when_measurement_above: {round_trip_milliseconds: -1}, alert_severity: warn}":  "check_groups.c.fail_when_measurement_above.round_trip_milliseconds: expected a number of 0 or more",
		"{ping: db-1, fail_when_measurement_above: {round_trip_seconds: 1}, alert_severity: warn}":        "check_groups.c.fail_when_measurement_above.round_trip_seconds: this check has no such measurement; it measures round_trip_milliseconds",
		"{ping: 'http://db-1', alert_severity: warn}":                                                     "check_groups.c.ping: must be the name or the address of a machine, such as db-1.internal.example.com or 10.0.0.5",
		"{ping: 'db 1', alert_severity: warn}":                                                            "check_groups.c.ping: must be the name or the address of a machine",
		"{ping: '', alert_severity: warn}":                                                                "check_groups.c.ping: must be the name or the address of a machine",
		"{ping: db-1}":                                                                                    "check_groups.c.alert_severity: required: info, warn, or sos",
		// A module that records a measurement and takes no limit on it says so.
		"{tls: {fail_when_certificate_expires_within: 30d, address: 'mail.example.com:465'}, fail_when_measurement_below: {certificate_expires_in_seconds: 1}, alert_severity: warn}":                      "check_groups.c.fail_when_measurement_below: the module tls records its measurements but takes no limit on them",
		"{systemd_unit_crash_loop: {systemd_unit: myapp.service, automatic_restarts_above: 2, automatic_restarts_within: 1h}, fail_when_measurement_above: {automatic_restarts: 1}, alert_severity: warn}": "check_groups.c.fail_when_measurement_above: the module systemd_unit_crash_loop records its measurements but takes no limit on them",
	})
}

// A ping request is an ICMP echo request, and its answer carries the same
// marker and the number of the request.
func TestPingRequestAndReply(t *testing.T) {
	t.Parallel()
	marker := []byte{0x12, 0x34, 1, 2, 3, 4, 5, 6, 7, 8}
	request := primitives.EchoRequest(8, marker, 2)
	if want := []byte{8, 0, 0, 0, 0x12, 0x34, 0, 2, 1, 2, 3, 4, 5, 6, 7, 8}; len(request) != 16 || !reflect.DeepEqual(append(append([]byte{}, request[:2]...), request[4:]...), append(append([]byte{}, want[:2]...), want[4:]...)) {
		t.Fatal(request)
	}
	// A message with its checksum in place sums to zero.
	if primitives.InternetChecksum(request) != 0 || (request[2] == 0 && request[3] == 0) {
		t.Fatal("the request has no valid checksum", request)
	}
	// The checksum of the Internet protocols, by the example of RFC 1071.
	if got := primitives.InternetChecksum([]byte{0x00, 0x01, 0xf2, 0x03, 0xf4, 0xf5, 0xf6, 0xf7}); got != 0x220d {
		t.Fatalf("%#x", got)
	}
	if got := primitives.InternetChecksum([]byte{0xff}); got != 0x00ff {
		t.Fatalf("%#x", got)
	}
	// The kernel fills in the checksum of a request of IP version 6.
	if six := primitives.EchoRequest(128, marker, 0); six[0] != 128 || six[2] != 0 || six[3] != 0 {
		t.Fatal(six)
	}
	reply := append([]byte{}, request...)
	reply[0] = 0
	if sequence, valid := primitives.EchoReplySequence(reply, 0, marker); !valid || sequence != 2 {
		t.Fatal(sequence, valid)
	}
	for name, change := range map[string]func(m []byte) []byte{
		"a request of another program":          func(m []byte) []byte { m[4] = 0x99; return m },
		"another marker":                        func(m []byte) []byte { m[9] = 0x99; return m },
		"an echo request, not a reply":          func(m []byte) []byte { m[0] = 8; return m },
		"a number that MMWatchdog did not send": func(m []byte) []byte { m[7] = primitives.PingRequests; return m },
		"a shorter message":                     func(m []byte) []byte { return m[:12] },
		"an error message of a router":          func(m []byte) []byte { m[0], m[1] = 3, 1; return m },
	} {
		if sequence, valid := primitives.EchoReplySequence(change(append([]byte{}, reply...)), 0, marker); valid {
			t.Errorf("%s counted as the answer to request %d", name, sequence)
		}
	}
	// An address is its own answer, and needs no resolver.
	if addresses, err := primitives.ResolveHostName(context.Background(), "10.0.0.5"); err != nil || !reflect.DeepEqual(addresses, []netip.Addr{netip.MustParseAddr("10.0.0.5")}) {
		t.Fatal(addresses, err)
	}
}

// A ping check on this machine: the machine answers itself. Only root may
// send the request; another account gets an observation that says so.
func TestPingCheckOnThisMachine(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	check := checksandalerts.Check{Inputs: new(checksandalerts.Ping("127.0.0.1")), FailWhenMeasurementAbove: map[string]float64{"round_trip_milliseconds": 1000}}
	o := checksandalerts.RunCheck(ctx, checksandalerts.NewCheckRun(check, checksandalerts.Timing{TimeLimitPerRun: "3s"}))
	if os.Geteuid() != 0 {
		if o.Status != "unknown" || o.Message != "MMWatchdog needs the rights of root to send a ping request" {
			t.Fatal(o)
		}
		t.Skip("only root may send a ping request")
	}
	reading := o.Measurements["round_trip_milliseconds"]
	if o.Status != "ok" || reading.Unit != "milliseconds" || reading.Value <= 0 || reading.Value > 1000 ||
		!strings.HasPrefix(o.Message, "round_trip_milliseconds is ") || !strings.Contains(o.Message, " (fails above 1000); 127.0.0.1 answered a ping request in ") {
		t.Fatal(o)
	}
	check.FailWhenMeasurementAbove = map[string]float64{"round_trip_milliseconds": 0}
	if o := checksandalerts.RunCheck(ctx, checksandalerts.NewCheckRun(check, checksandalerts.Timing{})); o.Status != "failed" || !strings.Contains(o.Message, " (fails above 0); 127.0.0.1 answered") {
		t.Fatal(o)
	}
	// The name of this machine in the message, with the address.
	if o := checksandalerts.ObserveCheck(ctx, checksandalerts.Check{Inputs: new(checksandalerts.Ping("localhost"))}); o.Status != "ok" || !strings.HasPrefix(o.Message, "localhost (") {
		t.Fatal(o)
	}
	// A name that has no address fails the check.
	if o := checksandalerts.ObserveCheck(ctx, checksandalerts.Check{Inputs: new(checksandalerts.Ping("no-such-machine.invalid"))}); o.Status != "failed" || !strings.HasPrefix(o.Message, "MMWatchdog cannot find the address of no-such-machine.invalid: ") {
		t.Fatal(o)
	}
}

// A out_of_memory_kills check fails in a run when the kernel's count of
// processes that it ended to free memory rose since the run before. The
// kernel's log names each process.
func TestOutOfMemoryKillsCheck(t *testing.T) {
	root := testhelpers.KernelFiles(t, map[string]string{"proc/vmstat": "nr_free_pages 1000\noom_kill 3\npgfault 5\n", "proc/sys/kernel/random/boot_id": "boot-one\n"})
	vmstat := func(kills int) {
		os.WriteFile(filepath.Join(root, "proc/vmstat"), []byte(fmt.Sprintf("nr_free_pages 1000\noom_kill %d\npgfault 5\n", kills)), 0644)
	}
	logged, asked := filepath.Join(t.TempDir(), "logged"), filepath.Join(t.TempDir(), "asked")
	fakeProgram(t, "journalctl", "echo \"$@\" > "+testhelpers.ShellQuote(asked)+"; cat "+testhelpers.ShellQuote(logged))
	os.WriteFile(logged, nil, 0644)
	runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.OutOfMemoryKills{FailWhen: "the_kernel_kills_a_process_for_lack_of_memory"}}}
	// The first run only notes the count: three kills from before.
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" || o.Message != "the kernel has ended no process to free memory since this check began to watch" {
		t.Fatal(o)
	}
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" || o.Message != "the kernel ended no process to free memory since the run before" {
		t.Fatal(o)
	}
	// One kill: the message names the process and the time.
	at := time.Date(2026, 10, 7, 3, 12, 30, 0, time.UTC)
	entry := func(message string, at time.Time) string {
		b, _ := json.Marshal(map[string]any{"MESSAGE": message, "__REALTIME_TIMESTAMP": fmt.Sprint(at.UnixMicro()), "PRIORITY": "3"})
		return string(b) + "\n"
	}
	killed := entry("Out of memory: Killed process 1234 (mysqld) total-vm:1000kB, anon-rss:900kB, file-rss:0kB, shmem-rss:0kB, UID:106 pgtables:50kB oom_score_adj:0", at)
	os.WriteFile(logged, []byte(entry("usb 1-1: new high-speed USB device", at)+killed), 0644)
	vmstat(4)
	before := time.Now().Unix()
	o := runs.run(testhelpers.TestContext(t))
	if o.Status != "failed" || !strings.HasPrefix(o.Message, "the kernel ended mysqld (process 1234) at ") || !strings.HasSuffix(o.Message, "03:12 UTC to free memory") || o.Details != nil {
		t.Fatal(o)
	}
	// MMWatchdog asks for the kernel's log since the run before.
	arguments, _ := os.ReadFile(asked)
	var since int64
	if _, err := fmt.Sscanf(string(arguments), "--no-pager --dmesg --output=json --all --output-fields=MESSAGE --since=@%d", &since); err != nil || since < before-3 || since > before {
		t.Fatalf("%q %v", arguments, err)
	}
	// Two kills, one by the memory limit of a control group, and an older
	// one that the log still holds: the details name the two newest.
	os.WriteFile(logged, []byte(killed+entry("Memory cgroup out of memory: Killed process 77 (python3) total-vm:5kB", at.Add(time.Minute))+
		entry("Out of memory: Killed process 88 (java (worker)) total-vm:5kB", at.Add(2*time.Minute))), 0644)
	vmstat(6)
	o = runs.run(testhelpers.TestContext(t))
	if o.Status != "failed" || o.Message != "the kernel ended 2 processes to free memory since the run before" || len(o.Details) != 2 ||
		!strings.HasPrefix(o.Details[0], "the kernel ended python3 (process 77) at ") || !strings.HasSuffix(o.Details[0], "03:13 UTC to free memory, because its control group reached its memory limit") ||
		!strings.HasPrefix(o.Details[1], "the kernel ended java (worker) (process 88) at ") {
		t.Fatal(o)
	}
	// The next run without a new kill is healthy again.
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" {
		t.Fatal(o)
	}
	// The kernel counts from zero in a new boot: every kill of that boot is
	// new, also when the count is lower than the last one.
	os.WriteFile(filepath.Join(root, "proc/sys/kernel/random/boot_id"), []byte("boot-two\n"), 0644)
	vmstat(1)
	os.WriteFile(logged, nil, 0644)
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "failed" || o.Message != "the kernel ended 1 process to free memory since the run before" || !reflect.DeepEqual(o.Details, []string{"the kernel's log does not name the processes"}) {
		t.Fatal(o)
	}
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" {
		t.Fatal(o)
	}
	// When MMWatchdog cannot read the kernel's log, the check still fails, and says
	// that the names are missing.
	fakeProgram(t, "journalctl", "echo 'No journal files were found.' >&2; exit 1")
	vmstat(2)
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "failed" || len(o.Details) != 1 || !strings.HasPrefix(o.Details[0], "MMWatchdog could not read the names of the processes from the kernel's log: exit status 1: No journal files were found.") {
		t.Fatal(o)
	}
	// A run of mmwatchdog check-now counts from what the scheduled runs stored and
	// stores nothing itself.
	vmstat(5)
	diagnostic := &checksandalerts.CheckRun{Check: runs.check, Memory: &checksandalerts.CheckMemory{}, State: &checksandalerts.ModuleState{Stored: runs.stored}, Diagnostic: true}
	if o := checksandalerts.RunCheck(testhelpers.TestContext(t), diagnostic); o.Status != "failed" || !strings.HasPrefix(o.Message, "the kernel ended 3 processes") {
		t.Fatal(o)
	}
	var kept checksandalerts.OutOfMemoryKillsSeen
	if json.Unmarshal(runs.stored, &kept); kept.Kills != 2 || kept.BootID != "boot-two" {
		t.Fatal(kept)
	}
	// A kernel that does not count them makes the check unknown.
	os.WriteFile(filepath.Join(root, "proc/vmstat"), []byte("nr_free_pages 1000\n"), 0644)
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "unknown" || !strings.Contains(o.Message, "/proc/vmstat has no oom_kill") {
		t.Fatal(o)
	}
	if lines := checksandalerts.OutOfMemoryKillsInLog("not JSON\n"+entry("Killed process 5 (old-kernel), UID 0, total-vm:1kB", at)+`{"MESSAGE":[75,105,108,108,101,100],"__REALTIME_TIMESTAMP":"1"}`+"\n", at); len(lines) != 1 || lines[0] != "the kernel ended old-kernel (process 5) at 03:12 UTC to free memory" {
		t.Fatal(lines)
	}
}

// A file check looks at one file: that it exists, its owner, its group, its
// permissions, its content, its age, and its size.
func TestFileCheck(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	path := filepath.Join(directory, "database.sql.gz")
	observe := func(file checksandalerts.File, check checksandalerts.Check) checksandalerts.Observation {
		file.Path = path
		check.Inputs = &file
		return checksandalerts.ObserveCheck(context.Background(), check)
	}
	if o := observe(checksandalerts.File{}, checksandalerts.Check{}); o.Status != "failed" || o.Message != path+" does not exist" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.File{MustNotExist: true}, checksandalerts.Check{}); o.Status != "ok" || o.Message != path+" does not exist" {
		t.Fatal(o)
	}
	// A file of 2 MiB that was last changed 3 hours ago.
	if err := os.WriteFile(path, make([]byte, 2*1024*1024), 0640); err != nil {
		t.Fatal(err)
	}
	changed := time.Now().Add(-3 * time.Hour)
	os.Chtimes(path, changed, changed)
	o := observe(checksandalerts.File{}, checksandalerts.Check{})
	if o.Status != "ok" || o.Message != path+" exists" || o.Measurements["file_size_mib"] != (checksandalerts.Measurement{Value: 2, Unit: "MiB"}) || o.Measurements["file_age_hours"].Unit != "hours" ||
		o.Measurements["file_age_hours"].Value < 3 || o.Measurements["file_age_hours"].Value > 3.01 || o.Measurements["file_age_minutes"].Value < 180 || o.Measurements["file_age_days"].Value > 0.126 || o.Measurements["file_age_days"].Unit != "days" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.File{MustNotExist: true}, checksandalerts.Check{}); o.Status != "failed" || o.Message != path+" exists" {
		t.Fatal(o)
	}
	fresh := checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"file_age_hours": 26}, FailWhenMeasurementBelow: map[string]float64{"file_size_mib": 1}}
	if o := observe(checksandalerts.File{}, fresh); o.Status != "ok" || !strings.HasPrefix(o.Message, "file_age_hours is 3 (fails above 26); file_size_mib is 2 (fails below 1); "+path+" exists") {
		t.Fatal(o)
	}
	fresh.FailWhenMeasurementAbove = map[string]float64{"file_age_hours": 2}
	if o := observe(checksandalerts.File{}, fresh); o.Status != "failed" || !strings.HasPrefix(o.Message, "file_age_hours is 3 (fails above 2); ") {
		t.Fatal(o)
	}
	fresh.FailWhenMeasurementBelow = map[string]float64{"file_size_mib": 3}
	if o := observe(checksandalerts.File{}, checksandalerts.Check{FailWhenMeasurementBelow: fresh.FailWhenMeasurementBelow}); o.Status != "failed" || !strings.HasPrefix(o.Message, "file_size_mib is 2 (fails below 3); ") {
		t.Fatal(o)
	}

	// What the file is expected to have. The account and the group of this
	// test own the file.
	about, err := primitives.InformationAboutFile(path)
	if err != nil || !about.IsRegularFile || about.Kind != "a regular file" || about.SizeBytes != 2*1024*1024 || about.PermissionsMode != 0640 || about.Inode == 0 || about.UserID != uint32(os.Getuid()) {
		t.Fatal(about, err)
	}
	owner, group := primitives.UserAccountName(context.Background(), about.UserID), primitives.GroupName(context.Background(), about.GroupID)
	// The account and the group of the test have names.
	if _, err := strconv.Atoi(owner); err == nil {
		t.Fatal(owner)
	}
	if _, err := strconv.Atoi(group); err == nil {
		t.Fatal(group)
	}
	const empty = "5647f05ec18958947d32874eeb788fa396a05d0bab7c1b71f112ceb7e9b31eee"
	matching := checksandalerts.File{ExpectedOwner: owner, ExpectedGroup: group, ExpectedPermissionsMode: "0640", ExpectedSHA256Checksum: empty}
	if o := observe(matching, checksandalerts.Check{}); o.Status != "ok" || o.Message != path+" is as expected" {
		t.Fatal(o)
	}
	// An account and a group can also be named by their numbers.
	if o := observe(checksandalerts.File{ExpectedOwner: fmt.Sprint(about.UserID), ExpectedGroup: fmt.Sprint(about.GroupID), ExpectedPermissionsMode: "640"}, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	}
	other := checksandalerts.File{ExpectedOwner: "no-such-account", ExpectedGroup: "no-such-group", ExpectedPermissionsMode: "0600", ExpectedSHA256Checksum: strings.Repeat("0", 64)}
	want := path + " is owned by the user account " + owner + ", expected no-such-account; " + path + " belongs to the group " + group + ", expected no-such-group; " +
		path + " has permissions mode 0640, expected 0600; " + path + " has the SHA-256 checksum " + empty + ", expected " + strings.Repeat("0", 64)
	if o := observe(other, checksandalerts.Check{}); o.Status != "failed" || o.Message != want || o.Measurements != nil {
		t.Fatalf("%s\n%s", o.Message, want)
	}
	os.Chmod(path, 0644)
	if o := observe(checksandalerts.File{ExpectedPermissionsMode: "0600"}, checksandalerts.Check{}); o.Status != "failed" || o.Message != path+" has permissions mode 0644, expected 0600" {
		t.Fatal(o)
	}
	os.Chmod(path, os.ModeSetuid|0755)
	if o := observe(checksandalerts.File{ExpectedPermissionsMode: "4755"}, checksandalerts.Check{}); o.Status != "ok" {
		t.Fatal(o)
	}
	// A file that MMWatchdog cannot read within the time limit makes the run unknown.
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if o := checksandalerts.RunCheck(ended, checksandalerts.NewCheckRun(checksandalerts.Check{Inputs: &checksandalerts.File{Path: path, ExpectedSHA256Checksum: empty}}, checksandalerts.Timing{})); o.Status != "unknown" || o.Message != "MMWatchdog could not read all of "+path+" within the time limit of the run" {
		t.Fatal(o)
	}
	// A file from the future has no age: only a limit on its age is unknown.
	future := time.Now().Add(time.Hour)
	os.Chtimes(path, future, future)
	if o := observe(checksandalerts.File{}, checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"file_age_hours": 26}}); o.Status != "unknown" || !strings.HasPrefix(o.Message, path+" was last changed at ") || !strings.HasSuffix(o.Message, ", which is in the future, so its age cannot be measured") {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.File{}, checksandalerts.Check{FailWhenMeasurementBelow: map[string]float64{"file_size_mib": 1}}); o.Status != "ok" || len(o.Measurements) != 1 {
		t.Fatal(o)
	}
	// A link stands for the file that it names.
	link := filepath.Join(directory, "link")
	os.Symlink(path, link)
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.File{Path: link, ExpectedSHA256Checksum: empty}}); o.Status != "ok" {
		t.Fatal(o)
	}
	os.Remove(path)
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.File{Path: link}}); o.Status != "failed" || o.Message != link+" does not exist" {
		t.Fatal(o)
	}

	loadChecks(t, map[string]string{
		"{file: {path: /var/backups/db.gz}, fail_when_measurement_above: {file_age_hours: 26}, fail_when_measurement_below: {file_size_mib: 1}, alert_severity: sos}":                          "",
		"{file: {path: /etc/ssh/sshd_config, expected_owner: root, expected_group: root, expected_permissions_mode: '0600', expected_sha256_checksum: '" + empty + "'}, alert_severity: warn}": "",
		"{file: {path: /srv/myapp/maintenance.flag, must_not_exist: true}, alert_severity: warn}":                                                                                              "",
		"{file: {}, alert_severity: warn}":                                                     "check_groups.c.file.path: is required: the absolute path of the file or directory that the check looks at",
		"{file: {path: backups/db.gz}, alert_severity: warn}":                                  "check_groups.c.file.path: is required: the absolute path",
		"{file: /var/backups/db.gz, alert_severity: warn}":                                     "expected a mapping",
		"{file: {path: /a}}":                                                                   "check_groups.c.alert_severity: required: info, warn, or sos",
		"{file: {path: /a, must_not_exist: true, expected_owner: root}, alert_severity: warn}": "check_groups.c.file.expected_owner: a check with must_not_exist: true looks only at whether the file exists; remove expected_owner",
		"{file: {path: /a, must_not_exist: true}, fail_when_measurement_above: {file_age_hours: 1}, alert_severity: warn}": "check_groups.c.fail_when_measurement_above: a file check with must_not_exist: true looks only at whether the file exists; remove fail_when_measurement_above",
		"{file: {path: /a, expected_permissions_mode: 0600}, alert_severity: warn}":                                        "expected a string",
		"{file: {path: /a, expected_permissions_mode: 'rw-------'}, alert_severity: warn}":                                 "check_groups.c.file.expected_permissions_mode: expected the permissions as chmod writes them, in quotes, such as \"0600\"",
		"{file: {path: /a, expected_permissions_mode: '0888'}, alert_severity: warn}":                                      "check_groups.c.file.expected_permissions_mode: expected the permissions as chmod writes them",
		"{file: {path: /a, expected_sha256_checksum: abc}, alert_severity: warn}":                                          "check_groups.c.file.expected_sha256_checksum: expected a SHA-256 checksum: 64 hex characters in lower case, as sha256sum prints them",
		"{file: {path: /a, expected_owner: ''}, alert_severity: warn}":                                                     "check_groups.c.file.expected_owner: cannot be empty and holds no white space",
		"{file: {path: /a, mode: '0600'}, alert_severity: warn}":                                                           `unknown field "mode"`,
		"{file: {path: /a}, fail_when_measurement_above: {age_seconds: 5}, alert_severity: warn}":                          "check_groups.c.fail_when_measurement_above.age_seconds: this check has no such measurement; it measures file_age_minutes, file_age_hours, file_age_days, file_size_mib",
	})
}

// logFile is a log file that a test adds lines to.
type logFile struct {
	t    *testing.T
	path string
}

func (l logFile) add(text string) {
	l.t.Helper()
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		l.t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString(text); err != nil {
		l.t.Fatal(err)
	}
}

// A log_lines check of a file reads only the lines that are new since the
// run before, remembers how far it has read, starts at the end of the log,
// and reads a rotated file from its start.
func TestLogLinesCheckOfAFile(t *testing.T) {
	t.Parallel()
	log := logFile{t, filepath.Join(t.TempDir(), "application.log")}
	runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: log.path, FailWhenANewLineContains: "FATAL"}}}
	if o := runs.run(context.Background()); o.Status != "unknown" || o.Message != log.path+" does not exist" {
		t.Fatal(o)
	}
	// The first run starts at the end: a line from before causes no alert.
	log.add("FATAL: an old problem\n")
	if o := runs.run(context.Background()); o.Status != "ok" || o.Message != "MMWatchdog starts watching new lines in "+log.path+" from now on" {
		t.Fatal(o)
	}
	if o := runs.run(context.Background()); o.Status != "ok" || o.Message != `none of 0 new lines in `+log.path+` contains "FATAL"` {
		t.Fatal(o)
	}
	log.add("INFO: started\nWARN: slow\r\n")
	if o := runs.run(context.Background()); o.Status != "ok" || o.Message != `none of 2 new lines in `+log.path+` contains "FATAL"` {
		t.Fatal(o)
	}
	// A line counts once the program has written its end.
	log.add("FATAL: cannot open")
	if o := runs.run(context.Background()); o.Status != "ok" {
		t.Fatal(o)
	}
	log.add(" database\x1b[2J\nINFO: retrying\n")
	o := runs.run(context.Background())
	if o.Status != "failed" || o.Message != `1 new line in `+log.path+` contains "FATAL"` || !reflect.DeepEqual(o.Details, []string{"FATAL: cannot open database [2J"}) {
		t.Fatal(o)
	}
	// The next run reads nothing twice and is healthy again.
	if o := runs.run(context.Background()); o.Status != "ok" {
		t.Fatal(o)
	}
	// The body shows the first five matching lines and counts the rest.
	for i := range 8 {
		log.add(fmt.Sprintf("FATAL: problem %d\n", i+1))
	}
	o = runs.run(context.Background())
	if want := []string{"FATAL: problem 1", "FATAL: problem 2", "FATAL: problem 3", "FATAL: problem 4", "FATAL: problem 5", "and 3 more"}; o.Status != "failed" || o.Message != "8 new lines in "+log.path+` contain "FATAL"` || !reflect.DeepEqual(o.Details, want) {
		t.Fatal(o)
	}
	// A run of mmwatchdog check-now reads from where the scheduled runs stopped and
	// moves nothing.
	log.add("FATAL: seen by check-now\n")
	for range 2 {
		diagnostic := &checksandalerts.CheckRun{Check: runs.check, Memory: &checksandalerts.CheckMemory{}, State: &checksandalerts.ModuleState{Stored: runs.stored}, Diagnostic: true}
		if o := checksandalerts.RunCheck(context.Background(), diagnostic); o.Status != "failed" || !reflect.DeepEqual(o.Details, []string{"FATAL: seen by check-now"}) {
			t.Fatal(o)
		}
	}
	if o := runs.run(context.Background()); o.Status != "failed" {
		t.Fatal(o)
	}
	// A rotated log: the program starts a fresh file under the same name,
	// and MMWatchdog reads that file from its start.
	if err := os.Rename(log.path, log.path+".1"); err != nil {
		t.Fatal(err)
	}
	log.add("FATAL: first line of the new file\nINFO: more\n")
	if o := runs.run(context.Background()); o.Status != "failed" || !reflect.DeepEqual(o.Details, []string{"FATAL: first line of the new file"}) {
		t.Fatal(o)
	}
	// A log that was emptied in place is read from its start too.
	if err := os.Truncate(log.path, 0); err != nil {
		t.Fatal(err)
	}
	log.add("FATAL: x\n")
	if o := runs.run(context.Background()); o.Status != "failed" || !reflect.DeepEqual(o.Details, []string{"FATAL: x"}) {
		t.Fatal(o)
	}
	// A run that cannot read all new lines in time is unknown, unless it
	// already found a line: the next run continues.
	log.add("INFO: a\nINFO: b\n")
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if o := runs.run(ended); o.Status != "unknown" || !strings.HasPrefix(o.Message, log.path+" has more new lines than MMWatchdog reads in one run, or than MMWatchdog could read within the time limit") {
		t.Fatal(o)
	}
	if o := runs.run(context.Background()); o.Status != "ok" || !strings.HasPrefix(o.Message, "none of 2 new lines") {
		t.Fatal(o)
	}

	// A regular expression.
	pattern := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: log.path, FailWhenANewLineMatchesRegex: `^(ERROR|FATAL): .* \d+$`}}}
	pattern.run(context.Background())
	log.add("ERROR: no number\nINFO: ERROR: code 7\nERROR: code 7\n")
	if o := pattern.run(context.Background()); o.Status != "failed" || o.Message != "1 new line in "+log.path+` matches the regular expression ^(ERROR|FATAL): .* \d+$` || !reflect.DeepEqual(o.Details, []string{"ERROR: code 7"}) {
		t.Fatal(o)
	}
	log.add("ERROR: code 8\nFATAL: code 9\n")
	if o := pattern.run(context.Background()); o.Status != "failed" || o.Message != "2 new lines in "+log.path+` match the regular expression ^(ERROR|FATAL): .* \d+$` {
		t.Fatal(o)
	}
	// Something else at the path makes the check unknown.
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: filepath.Dir(log.path), FailWhenANewLineContains: "x"}}); o.Status != "unknown" || !strings.HasSuffix(o.Message, " is a directory, not a regular file") {
		t.Fatal(o)
	}
}

// A log_lines check with fail_when_no_new_line_for fails when its source
// got no new line for that long, counted from the newest line that MMWatchdog has
// seen, or from the check's first run when it has seen none.
func TestLogLinesCheckOfASilentLog(t *testing.T) {
	t.Parallel()
	log := logFile{t, filepath.Join(t.TempDir(), "worker.log")}
	log.add("started\n")
	runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: log.path, FailWhenNoNewLineFor: "10m"}}}
	if o := runs.run(context.Background()); o.Status != "ok" {
		t.Fatal(o)
	}
	if o := runs.run(context.Background()); o.Status != "ok" || !strings.HasPrefix(o.Message, log.path+" received 0 new lines; no new line in the last 0s (fails after 10m)") {
		t.Fatal(o)
	}
	// The check began to read 11 minutes ago and has seen no line.
	age := func(change func(*checksandalerts.LogLinesRead)) {
		var read checksandalerts.LogLinesRead
		if err := json.Unmarshal(runs.stored, &read); err != nil {
			t.Fatal(err)
		}
		change(&read)
		runs.stored, _ = json.Marshal(read)
	}
	age(func(read *checksandalerts.LogLinesRead) {
		read.WatchingSince = read.WatchingSince.Add(-11 * time.Minute)
	})
	// The message has the seconds that the test itself took, so the test
	// reads the minutes only.
	if o := runs.run(context.Background()); o.Status != "failed" || !regexp.MustCompile(`^`+regexp.QuoteMeta(log.path)+` got no new line for 11m\d+s since MMWatchdog started watching the log \(fails after 10m\)$`).MatchString(o.Message) {
		t.Fatal(o)
	}
	// A new line ends the silence. Its time is the time of the write.
	log.add("working\n")
	if o := runs.run(context.Background()); o.Status != "ok" || !strings.HasPrefix(o.Message, log.path+" received 1 new line; no new line in the last ") {
		t.Fatal(o)
	}
	age(func(read *checksandalerts.LogLinesRead) { read.NewestLineAt = read.NewestLineAt.Add(-15 * time.Minute) })
	if o := runs.run(context.Background()); o.Status != "failed" || !regexp.MustCompile(`^`+regexp.QuoteMeta(log.path)+` got no new line for 15m\d+s since the newest line \(fails after 10m\)$`).MatchString(o.Message) {
		t.Fatal(o)
	}
	// A backlog alone is not evidence of recent activity.
	log.add("working\nworking\n")
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if o := runs.run(ended); o.Status != "unknown" || !strings.Contains(o.Message, "context canceled") {
		t.Fatal(o)
	}
}

// The file under the name of a log can be another one than at the run
// before. A log_lines check misses no line of the old file, and reads no
// line twice.
func TestLogLinesCheckOfAFileThatIsReplaced(t *testing.T) {
	t.Parallel()
	log := logFile{t, filepath.Join(t.TempDir(), "application.log")}
	log.add("INFO: started\n")
	runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: log.path, FailWhenANewLineContains: "FATAL"}}}
	details := func(want ...string) {
		t.Helper()
		o := runs.run(context.Background())
		if len(want) == 0 && o.Status == "ok" {
			return
		}
		if o.Status != "failed" || !reflect.DeepEqual(o.Details, want) {
			t.Fatal(o)
		}
	}
	details()
	// The program writes its last words and a line without an end, and
	// then starts a new file under the name. The run after that reads the
	// end of the old file first, and then the new file from its start.
	log.add("FATAL: out of disk, shutting down\nFATAL: half a line")
	if err := os.Rename(log.path, log.path+".1"); err != nil {
		t.Fatal(err)
	}
	log.add("INFO: started again\nFATAL: in the new file\n")
	details("FATAL: out of disk, shutting down", "FATAL: half a line", "FATAL: in the new file")
	details()
	// The same when the old file is removed and not only renamed.
	log.add("FATAL: before the file is removed\n")
	if err := os.Remove(log.path); err != nil {
		t.Fatal(err)
	}
	log.add("FATAL: after\n")
	details("FATAL: before the file is removed", "FATAL: after")
	// A file that is gone from its name, with no new file there yet: the
	// run reports the last lines of the old file, the next run says that
	// the name holds no file, and MMWatchdog keeps the old file open no longer.
	log.add("FATAL: the last line\n")
	if err := os.Remove(log.path); err != nil {
		t.Fatal(err)
	}
	details("FATAL: the last line")
	if o := runs.run(context.Background()); o.Status != "unknown" || o.Message != log.path+" does not exist" || runs.memory.Value != nil {
		t.Fatal(o, runs.memory.Value)
	}
	log.add("FATAL: a file is there again\n")
	details("FATAL: a file is there again")

	// A file whose content was replaced in place and has grown past the
	// place that the run before reached: the file starts in another way
	// than before, so MMWatchdog reads it from its start.
	log.add("INFO: one\nINFO: two\n")
	details()
	if err := os.WriteFile(log.path, []byte("FATAL: the first line of other content\n"+strings.Repeat("INFO: filler\n", 20)), 0644); err != nil {
		t.Fatal(err)
	}
	details("FATAL: the first line of other content")

	// After a start of MMWatchdog the check has only what it stored. It knows the
	// file again and reads on from where it stopped.
	runs.memory = nil
	log.add("FATAL: written while MMWatchdog was stopped\n")
	details("FATAL: written while MMWatchdog was stopped")
	// A new file under the name while MMWatchdog was stopped is read from its
	// start, and so is a file with the same inode and another start.
	runs.memory = nil
	if err := os.Rename(log.path, log.path+".2"); err != nil {
		t.Fatal(err)
	}
	log.add("FATAL: a new file while MMWatchdog was stopped\n")
	details("FATAL: a new file while MMWatchdog was stopped")
	runs.memory = nil
	if err := os.WriteFile(log.path, []byte("FATAL: other content while MMWatchdog was stopped\n"+strings.Repeat("INFO: filler\n", 20)), 0644); err != nil {
		t.Fatal(err)
	}
	details("FATAL: other content while MMWatchdog was stopped")
	// What the check stores names no device: a device can get another
	// number when the machine starts.
	if strings.Contains(string(runs.stored), "device") || !strings.Contains(string(runs.stored), `"start_checksum"`) {
		t.Fatal(string(runs.stored))
	}

	// A run keeps no line that does not match: two million lines of a
	// burst are counted and let go.
	log.add(strings.Repeat("\n", 2*1024*1024) + "FATAL: after the burst\n")
	if o := runs.run(context.Background()); o.Status != "failed" || o.Message != `1 new line in `+log.path+` contains "FATAL"` {
		t.Fatal(o)
	}
	// An oversized line checks its first 64 KiB and skips the remainder.
	log.add("FATAL " + strings.Repeat("x", 3*1024*1024) + "\nINFO: " + strings.Repeat("y", 70*1024) + " FATAL: too far into its line\n")
	if o := runs.run(context.Background()); o.Status != "failed" || !strings.Contains(o.Message, "exceeds 64 KiB") {
		t.Fatal(o.Status, o.Message, len(o.Details))
	}

	// A named pipe under the name holds no run up: nobody writes to it,
	// and the check says what it found.
	pipe := filepath.Join(t.TempDir(), "pipe.log")
	if err := syscall.Mkfifo(pipe, 0644); err != nil {
		t.Fatal(err)
	}
	if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: pipe, FailWhenANewLineContains: "x"}}); o.Status != "unknown" || o.Message != pipe+" is a named pipe, not a regular file" {
		t.Fatal(o)
	}
	if _, about, err := primitives.OpenRegularFile(pipe); !errors.Is(err, primitives.ErrNotARegularFile) || about.Kind != "a named pipe" {
		t.Fatal(about, err)
	}
	for _, file := range []checksandalerts.File{{Path: pipe}, {Path: pipe, ExpectedSHA256Checksum: strings.Repeat("0", 64)}} {
		if o := checksandalerts.ObserveCheck(context.Background(), checksandalerts.Check{Inputs: &file}); o.Status != "failed" || o.Message != pipe+" is a named pipe, not a regular file or a directory" {
			t.Fatal(o)
		}
	}
}

// A file check of a directory looks at whether it exists, its owner, its
// group, its permissions, and its age, which counts from the last entry
// added, removed, or renamed in it. A directory has no content to compare
// with a checksum and no size, so a check that asks for either fails and
// names the input.
func TestFileCheckOfADirectory(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "cron.d")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	threeHoursAgo := time.Now().Add(-3 * time.Hour)
	os.Chtimes(path, threeHoursAgo, threeHoursAgo)
	about, err := primitives.InformationAboutFile(path)
	if err != nil || !about.IsDirectory || about.IsRegularFile {
		t.Fatal(about, err)
	}
	owner, group := primitives.UserAccountName(context.Background(), about.UserID), primitives.GroupName(context.Background(), about.GroupID)
	observe := func(file checksandalerts.File, check checksandalerts.Check) checksandalerts.Observation {
		file.Path = path
		check.Inputs = &file
		return checksandalerts.ObserveCheck(context.Background(), check)
	}
	if o := observe(checksandalerts.File{}, checksandalerts.Check{}); o.Status != "ok" || o.Message != path+" exists" || o.Measurements["file_age_hours"].Value < 2.9 {
		t.Fatal(o)
	}
	if _, measured := observe(checksandalerts.File{}, checksandalerts.Check{}).Measurements["file_size_mib"]; measured {
		t.Fatal("a directory has no size")
	}
	if o := observe(checksandalerts.File{ExpectedOwner: owner, ExpectedGroup: group, ExpectedPermissionsMode: "0755"}, checksandalerts.Check{}); o.Status != "ok" || o.Message != path+" is as expected" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.File{ExpectedPermissionsMode: "0700"}, checksandalerts.Check{}); o.Status != "failed" || o.Message != path+" has permissions mode 0755, expected 0700" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.File{}, checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"file_age_hours": 2}}); o.Status != "failed" || o.Message != "file_age_hours is 3 (fails above 2); "+path+" exists" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.File{ExpectedSHA256Checksum: strings.Repeat("0", 64)}, checksandalerts.Check{}); o.Status != "failed" || o.Message != path+" is a directory, and expected_sha256_checksum checks the content of a regular file" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.File{}, checksandalerts.Check{FailWhenMeasurementBelow: map[string]float64{"file_size_mib": 1}}); o.Status != "failed" || o.Message != path+" is a directory, and the limit on file_size_mib in fail_when_measurement_below checks the size of a regular file" {
		t.Fatal(o)
	}
	// The directory's age changes when an entry is added.
	os.WriteFile(filepath.Join(path, "backup"), nil, 0o644)
	if o := observe(checksandalerts.File{}, checksandalerts.Check{FailWhenMeasurementAbove: map[string]float64{"file_age_hours": 2}}); o.Status != "ok" {
		t.Fatal(o)
	}
	if o := observe(checksandalerts.File{MustNotExist: true}, checksandalerts.Check{}); o.Status != "failed" || o.Message != path+" exists" {
		t.Fatal(o)
	}
}

// A line of an observation's details shows no character that changes how
// a terminal or a mail program shows the text around it.
func TestADetailLineHoldsNoCharacterThatChangesHowTextIsShown(t *testing.T) {
	t.Parallel()
	got := primitives.DetailLine("FATAL \x1b[2J \u009b2J \u0085 \u2028 \u202e reversed \u2066 isolated \x7f end")
	if want := "FATAL  [2J  2J       reversed   isolated   end"; got != want {
		t.Fatalf("%q", got)
	}
}

// A program whose output MMWatchdog reads only the start of ends at the limit, and
// does not run on to its own end.
func TestAProgramEndsAtTheLimitOfItsStandardOutput(t *testing.T) {
	t.Parallel()
	started := time.Now()
	ran, err := primitives.RunCommand(context.Background(), primitives.Command{Arguments: []string{"/bin/sh", "-c", "while :; do echo a line of a very long output; done"}, AsMMWatchdogItself: true, StandardOutputLimit: 4096, EndsAtStandardOutputLimit: true})
	if !errors.Is(err, primitives.ErrStandardOutputLimit) || len(ran.StandardOutput) != 4096 || time.Since(started) > 20*time.Second {
		t.Fatal(err, len(ran.StandardOutput), time.Since(started))
	}
}

// The name of an account that is not in the node's own files comes from
// the node's getent program, which asks every source of accounts.
func TestAccountNamesOfADirectoryService(t *testing.T) {
	asThisAccount(t)
	fakeProgram(t, "getent", `case "$1 $2" in
"passwd 4000000001") echo "alice:*:4000000001:4000000002:Alice:/home/alice:/bin/bash" ;;
"group 4000000002") echo "developers:*:4000000002:alice" ;;
*) exit 2 ;;
esac`)
	if name := primitives.UserAccountName(testhelpers.TestContext(t), 4000000001); name != "alice" {
		t.Fatal(name)
	}
	if name := primitives.GroupName(testhelpers.TestContext(t), 4000000002); name != "developers" {
		t.Fatal(name)
	}
	// An ID that no source knows stays a number.
	if name := primitives.UserAccountName(testhelpers.TestContext(t), 4000000003); name != "4000000003" {
		t.Fatal(name)
	}
	if name := primitives.GroupName(testhelpers.TestContext(t), 4000000003); name != "4000000003" {
		t.Fatal(name)
	}
}

// A log_lines check of the journal of a systemd unit reads the entries
// after the last one that it has read, by the journal's own cursor.
func TestLogLinesCheckOfAJournal(t *testing.T) {
	t.Parallel()
	shown := testhelpers.FakeUnit(t, "LoadState=loaded\nActiveState=active\n")
	written, asked := filepath.Join(t.TempDir(), "written"), filepath.Join(t.TempDir(), "asked")
	fakeProgram(t, "journalctl", "echo \"$@\" >> "+testhelpers.ShellQuote(asked)+"; cat "+testhelpers.ShellQuote(written))
	entry := func(cursor, message string, at time.Time) string {
		b, _ := json.Marshal(map[string]any{"MESSAGE": message, "__CURSOR": cursor, "__REALTIME_TIMESTAMP": fmt.Sprint(at.UnixMicro()), "_SYSTEMD_UNIT": "myapp.service"})
		return string(b) + "\n"
	}
	lastQuestion := func() string {
		data, _ := os.ReadFile(asked)
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		return lines[len(lines)-1]
	}
	now := time.Now()
	os.WriteFile(written, []byte(entry("s=1;i=10", "FATAL: an old problem", now)), 0644)
	runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{JournalOfSystemdUnit: "myapp.service", FailWhenANewLineContains: "FATAL"}}}
	// The first run notes the place of the newest entry.
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" || o.Message != "MMWatchdog starts watching new lines in the journal of myapp.service from now on" || lastQuestion() != "--no-pager --quiet --output=json --all --output-fields=MESSAGE --unit=myapp.service --lines=1" {
		t.Fatal(o, lastQuestion())
	}
	os.WriteFile(written, []byte(entry("s=1;i=11", "INFO: request served", now)+entry("s=1;i=12", "FATAL: cannot open database", now)+"not JSON\n"), 0644)
	o := runs.run(testhelpers.TestContext(t))
	if o.Status != "failed" || o.Message != `1 new line in the journal of myapp.service contains "FATAL"` || !reflect.DeepEqual(o.Details, []string{"FATAL: cannot open database"}) ||
		lastQuestion() != "--no-pager --quiet --output=json --all --output-fields=MESSAGE --unit=myapp.service --after-cursor=s=1;i=10" {
		t.Fatal(o, lastQuestion())
	}
	os.WriteFile(written, nil, 0644)
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" || lastQuestion() != "--no-pager --quiet --output=json --all --output-fields=MESSAGE --unit=myapp.service --after-cursor=s=1;i=12" {
		t.Fatal(o, lastQuestion())
	}
	// A message that is no valid text arrives as the numbers of its bytes.
	os.WriteFile(written, []byte(`{"MESSAGE":[70,65,84,65,76,255],"__CURSOR":"s=1;i=13","__REALTIME_TIMESTAMP":"1"}`+"\n"), 0644)
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "failed" || len(o.Details) != 1 || !strings.HasPrefix(o.Details[0], "FATAL") {
		t.Fatal(o)
	}
	// A unit that has written nothing yet is read from the time of the
	// check's first run.
	os.WriteFile(written, nil, 0644)
	silent := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{JournalOfSystemdUnit: "myapp.service", FailWhenNoNewLineFor: "10m"}}}
	before := time.Now().Unix()
	silent.run(testhelpers.TestContext(t))
	if o := silent.run(testhelpers.TestContext(t)); o.Status != "ok" || !strings.HasPrefix(lastQuestion(), "--no-pager --quiet --output=json --all --output-fields=MESSAGE --unit=myapp.service --since=@") {
		t.Fatal(o, lastQuestion())
	}
	var since int64
	if _, err := fmt.Sscanf(lastQuestion(), "--no-pager --quiet --output=json --all --output-fields=MESSAGE --unit=myapp.service --since=@%d", &since); err != nil || since < before || since > time.Now().Unix() {
		t.Fatal(lastQuestion(), err)
	}
	// The silence counts from the time of the newest entry. The message
	// has the seconds that the test itself took, so the test reads the
	// minutes only.
	os.WriteFile(written, []byte(entry("s=1;i=20", "working", time.Now().Add(-12*time.Minute))), 0644)
	if o := silent.run(testhelpers.TestContext(t)); o.Status != "failed" || !regexp.MustCompile(`^the journal of myapp\.service got no new line for 12m\d+s since the newest line \(fails after 10m\)$`).MatchString(o.Message) {
		t.Fatal(o)
	}
	// A unit that systemd does not know, and a journal that MMWatchdog cannot read,
	// make the check unknown.
	os.WriteFile(shown, []byte("LoadState=not-found\n"), 0644)
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "unknown" || o.Message != "systemd unit myapp.service not found" {
		t.Fatal(o)
	}
	os.WriteFile(shown, []byte("LoadState=loaded\nActiveState=inactive\n"), 0644)
	fakeProgram(t, "journalctl", "echo 'Failed to open journal' >&2; exit 1")
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "unknown" || o.Message != "MMWatchdog cannot read the journal of myapp.service: Failed to open journal" {
		t.Fatal(o)
	}
}

// A log_lines check states exactly one source and exactly one condition.
func TestTheInputsOfALogLinesCheck(t *testing.T) {
	t.Parallel()
	loadChecks(t, map[string]string{
		"{log_lines: {log_file_path: /var/log/myapp.log, fail_when_a_new_line_contains: FATAL}, alert_severity: warn}":                              "",
		"{log_lines: {log_file_path: /var/log/myapp.log, fail_when_a_new_line_matches_regex: '^(ERROR|FATAL)'}, alert_severity: warn}":              "",
		"{log_lines: {journal_of_systemd_unit: myapp-worker.service, fail_when_no_new_line_for: 10m}, alert_severity: sos}":                         "",
		"{log_lines: {journal_of_systemd_unit: backup.timer, fail_when_a_new_line_contains: failed}, alert_severity: sos}":                          "",
		"{log_lines: {fail_when_a_new_line_contains: FATAL}, alert_severity: warn}":                                                                 "check_groups.c.log_lines: states exactly one source: log_file_path or journal_of_systemd_unit",
		"{log_lines: {log_file_path: /a.log, journal_of_systemd_unit: a.service, fail_when_a_new_line_contains: FATAL}, alert_severity: warn}":      "check_groups.c.log_lines: states exactly one source: log_file_path or journal_of_systemd_unit",
		"{log_lines: {log_file_path: /a.log}, alert_severity: warn}":                                                                                "check_groups.c.log_lines: states exactly one condition: fail_when_a_new_line_contains, fail_when_a_new_line_matches_regex, or fail_when_no_new_line_for",
		"{log_lines: {log_file_path: /a.log, fail_when_a_new_line_contains: FATAL, fail_when_no_new_line_for: 10m}, alert_severity: warn}":          "check_groups.c.log_lines: states exactly one condition",
		"{log_lines: {log_file_path: a.log, fail_when_a_new_line_contains: FATAL}, alert_severity: warn}":                                           "check_groups.c.log_lines.log_file_path: must be the absolute path of the log file",
		"{log_lines: {journal_of_systemd_unit: myapp, fail_when_a_new_line_contains: FATAL}, alert_severity: warn}":                                 "check_groups.c.log_lines.journal_of_systemd_unit: must be the name of a systemd unit with its kind, such as myapp.service",
		"{log_lines: {log_file_path: /a.log, fail_when_a_new_line_contains: ''}, alert_severity: warn}":                                             "check_groups.c.log_lines.fail_when_a_new_line_contains: must be a text on one line that is not empty",
		"{log_lines: {log_file_path: /a.log, fail_when_a_new_line_matches_regex: '('}, alert_severity: warn}":                                       "check_groups.c.log_lines.fail_when_a_new_line_matches_regex: is no valid regular expression: ",
		"{log_lines: {log_file_path: /a.log, fail_when_no_new_line_for: 0s}, alert_severity: warn}":                                                 "check_groups.c.log_lines.fail_when_no_new_line_for: expected a duration longer than 0s, such as 10m",
		"{log_lines: {log_file_path: /a.log, fail_when_no_new_line_for: soon}, alert_severity: warn}":                                               "expected a duration such as 30s, 5m, 26h, or 7d",
		"{log_lines: {log_file_path: /a.log, fail_when_a_new_line_contains: FATAL}}":                                                                "check_groups.c.alert_severity: required: info, warn, or sos",
		"{log_lines: {log_file_path: /a.log, fail_when_a_new_line_contains: FATAL}, fail_when_measurement_above: {lines: 1}, alert_severity: warn}": "the module log_lines reports no measurement, so its check takes no limit",
	})
}

// The system_log destination writes one line to the system log for each
// alert: the title with the alert's severity in front, under the name
// mmwatchdog-alert and with the priority of the alert's severity.
func TestSystemLogDestination(t *testing.T) {
	directory, err := os.MkdirTemp("/tmp", "mmwatchdog")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	real := primitives.SystemLogSocket
	primitives.SystemLogSocket = filepath.Join(directory, "journal.sock")
	defer func() { primitives.SystemLogSocket = real }()
	destination := checksandalerts.Destination{RemindEvery: "never", Inputs: &checksandalerts.SystemLog{}, TimeLimitPerDeliveryAttempt: "1m"}
	of := checksandalerts.DeliveryOf{Destination: destination}
	if result := of.VerifyDestination(context.Background()); !result.CanVerifyWithoutMessage || result.AcceptsAddressAndCredential || !strings.Contains(result.VerificationMessage, "could not connect") {
		t.Fatal(result)
	}
	sender := destination.Inputs.Sender(checksandalerts.DeliveryOf{Destination: destination})
	failure := checksandalerts.Event{ID: "e1", Type: "failed", AlertSeverity: "sos", Title: "web-3: myapp/http failed", TitleWithAlertSeverity: "SOS: web-3: myapp/http failed", Body: "the body is not written"}
	// A line that the journal does not take is a failed attempt.
	if err := sender.Send(context.Background(), failure); err == nil || !strings.HasPrefix(err.Error(), "MMWatchdog could not write the alert to the system log: ") {
		t.Fatal(err)
	}
	journal, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: primitives.SystemLogSocket, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if result := of.VerifyDestination(context.Background()); !result.CanVerifyWithoutMessage || !result.AcceptsAddressAndCredential || result.VerificationMessage != "verified connection to the system log socket" {
		t.Fatal(result)
	}

	entry := func() string {
		t.Helper()
		received := make([]byte, 4096)
		journal.SetReadDeadline(time.Now().Add(2 * time.Second))
		n, err := journal.Read(received)
		if err != nil {
			t.Fatal(err)
		}
		return string(received[:n])
	}
	for _, tc := range []struct {
		event checksandalerts.Event
		want  string
	}{
		{failure, "PRIORITY=2\nSYSLOG_IDENTIFIER=mmwatchdog-alert\nMESSAGE=SOS: web-3: myapp/http failed\n"},
		{checksandalerts.Event{Type: "failed", AlertSeverity: "warn", TitleWithAlertSeverity: "Warning: web-3: disk/node_filesystem failed"}, "PRIORITY=4\nSYSLOG_IDENTIFIER=mmwatchdog-alert\nMESSAGE=Warning: web-3: disk/node_filesystem failed\n"},
		{checksandalerts.Event{Type: "maintenance_on", AlertSeverity: "info", TitleWithAlertSeverity: "Info: web-3: maintenance on for the whole node"}, "PRIORITY=6\nSYSLOG_IDENTIFIER=mmwatchdog-alert\nMESSAGE=Info: web-3: maintenance on for the whole node\n"},
		{checksandalerts.Event{Type: "reminder", AlertSeverity: "sos", TitleWithAlertSeverity: "SOS: web-3: myapp/http still failing"}, "PRIORITY=2\nSYSLOG_IDENTIFIER=mmwatchdog-alert\nMESSAGE=SOS: web-3: myapp/http still failing\n"},
		// An alert that closes an incident is informational.
		{checksandalerts.Event{Type: "recovered", AlertSeverity: "sos", TitleWithAlertSeverity: "Resolved SOS: web-3: myapp/http is ok again"}, "PRIORITY=6\nSYSLOG_IDENTIFIER=mmwatchdog-alert\nMESSAGE=Resolved SOS: web-3: myapp/http is ok again\n"},
		{checksandalerts.Event{Type: "can_check_again", AlertSeverity: "warn", TitleWithAlertSeverity: "Resolved warning: web-3: MMWatchdog can check myapp/http again"}, "PRIORITY=6\nSYSLOG_IDENTIFIER=mmwatchdog-alert\nMESSAGE=Resolved warning: web-3: MMWatchdog can check myapp/http again\n"},
	} {
		if err := sender.Send(context.Background(), tc.event); err != nil {
			t.Fatal(err)
		}
		if got := entry(); got != tc.want {
			t.Errorf("%q\nwant %q", got, tc.want)
		}
	}
	// A message of several lines is written with its length, as the journal
	// reads it.
	if err := primitives.WriteToSystemLog(context.Background(), 5, "mmwatchdog-alert", "two\nlines"); err != nil {
		t.Fatal(err)
	}
	if got := entry(); got != "PRIORITY=5\nSYSLOG_IDENTIFIER=mmwatchdog-alert\nMESSAGE\n\x09\x00\x00\x00\x00\x00\x00\x00two\nlines\n" {
		t.Fatalf("%q", got)
	}

	// The destination has no inputs, and it takes the settings of every
	// destination except send_only_the_alert_title.
	configuration := testhelpers.ConfigFile(t, "check_groups: {a: {shell: 'true'}}")
	for destinations, want := range map[string]string{
		"destinations: {machine_log: {remind_every: never, system_log: {}, alert_severities: [info, warn, sos]}}":                                                              "",
		"destinations: {machine_log: {system_log: {}, alert_severities: [sos], remind_every: 4h}}":                                                                             "",
		"destinations: {machine_log: {remind_every: never, system_log: {identifier: mmwatchdog}, alert_severities: [sos]}}":                                                    `unknown field "identifier"`,
		"destinations: {machine_log: {system_log: {}, remind_every: never}}":                                                                                                   "destinations.machine_log.alert_severities",
		"destinations: {machine_log: {remind_every: never, system_log: {}, alert_severities: [sos], send_only_the_alert_title: true}}":                                         "destinations.machine_log.send_only_the_alert_title: a system_log destination takes no send_only_the_alert_title: it writes only the title with the alert's severity in front",
		"destinations: {machine_log: {remind_every: never, system_log: {}, alert_severities: [sos]}, second_log: {remind_every: never, system_log: {}, alert_severities: []}}": "",
	} {
		if err := os.WriteFile(filepath.Join(configuration, "alert_destinations.yml"), []byte(destinations+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		c, err := config.Load(configuration)
		if (want == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), want)) {
			t.Errorf("%s:\n got %v\nwant %q", destinations, err, want)
		}
		if err == nil && (c.Alerts.Destinations["machine_log"].Module().Name != "system_log" || len(c.Alerts.Destinations["machine_log"].Inputs.CredentialFiles()) != 0) {
			t.Errorf("%s: %+v", destinations, c.Alerts.Destinations["machine_log"])
		}
	}
	// A destination that was written as JSON reads back as the same one.
	written, err := json.Marshal(destination)
	var back checksandalerts.Destination
	if err == nil {
		err = json.Unmarshal(written, &back)
	}
	if err != nil || !strings.HasPrefix(string(written), `{"system_log":{},"time_limit_per_delivery_attempt":"1m"`) || !checksandalerts.SameDestination(destination, back) {
		t.Fatalf("%s %v %+v", written, err, back)
	}
}

func TestLogFileOversizedLinesAreCheckedAndSkipped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before, longLine, status string }{
		{"unmatched prefix", "", strings.Repeat("x", 65537) + "FATAL", "unknown"},
		{"matched prefix", "", "FATAL " + strings.Repeat("x", 65537), "failed"},
		{"match before problem", "FATAL before\n", strings.Repeat("x", 65537), "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			log := logFile{t, filepath.Join(t.TempDir(), "application.log")}
			log.add("start\n")
			runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: log.path, FailWhenANewLineContains: "FATAL"}}}
			runs.run(context.Background())
			log.add(tc.before + tc.longLine + "\n")
			o := runs.run(context.Background())
			if o.Status != tc.status {
				t.Fatal(o)
			}
			if tc.name != "matched prefix" && (!strings.Contains(o.Message, log.path) || !strings.Contains(o.Message, fmt.Sprintf("line at byte %d exceeds 64 KiB", len("start\n"+tc.before)))) {
				t.Fatal(o)
			}
			if tc.name == "match before problem" && !reflect.DeepEqual(o.Details, []string{"FATAL before"}) {
				t.Fatal(o)
			}
			var read checksandalerts.LogLinesRead
			if err := json.Unmarshal(runs.stored, &read); err != nil {
				t.Fatal(err)
			}
			if read.Offset != int64(len("start\n"+tc.before+tc.longLine+"\n")) {
				t.Fatal(read)
			}
			log.add("FATAL next\n")
			if o := runs.run(context.Background()); o.Status != "failed" || !reflect.DeepEqual(o.Details, []string{"FATAL next"}) {
				t.Fatal(o)
			}
		})
	}
}

func TestLogSilenceUsesTheTimeOfAnOldFileBacklog(t *testing.T) {
	t.Parallel()
	log := logFile{t, filepath.Join(t.TempDir(), "worker.log")}
	log.add("start\n")
	runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: log.path, FailWhenNoNewLineFor: "10m"}}}
	runs.run(context.Background())
	log.add(strings.Repeat("line\n", 4*1024*1024))
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(log.path, old, old); err != nil {
		t.Fatal(err)
	}
	if o := runs.run(context.Background()); o.Status != "failed" || !strings.Contains(o.Message, "since the newest line") {
		t.Fatal(o)
	}
}

func TestLogReadingRetriesFromTheReadErrorPosition(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "application.log")
	contents := "FATAL first\n" + strings.Repeat(strings.Repeat("x", 1023)+"\n", 2000) + "FATAL last\n"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	read := &checksandalerts.LogLinesRead{}
	remaining, consumed := len(contents), int64(0)
	matches := []string{}
	more, problem := checksandalerts.ReadLogLinesForTest(context.Background(), file, read, int64(len(contents)), &remaining, false, func(line string) bool {
		if consumed == 0 {
			if err := file.Close(); err != nil {
				t.Fatal(err)
			}
		}
		consumed += int64(len(line) + 1)
		if strings.Contains(line, "FATAL") {
			matches = append(matches, line)
			return true
		}
		return false
	})
	if !more || !strings.Contains(problem, "read error at byte") || read.Offset != consumed || read.Offset <= 0 || read.Offset >= int64(len(contents)) || !reflect.DeepEqual(matches, []string{"FATAL first"}) {
		t.Fatal(read, consumed, problem, matches)
	}
	position := read.Offset
	_, problem = checksandalerts.ReadLogLinesForTest(context.Background(), file, read, int64(len(contents)), &remaining, false, func(string) bool { t.Fatal("closed file produced a line"); return false })
	if problem == "" || read.Offset != position {
		t.Fatal(read, problem)
	}
	file, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	_, problem = checksandalerts.ReadLogLinesForTest(context.Background(), file, read, int64(len(contents)), &remaining, false, func(line string) bool {
		if strings.Contains(line, "FATAL") {
			matches = append(matches, line)
			return true
		}
		return false
	})
	if problem != "" || read.Offset != int64(len(contents)) || !reflect.DeepEqual(matches, []string{"FATAL first", "FATAL last"}) {
		t.Fatal(read, problem, matches)
	}
}

func TestLogJournalOversizedEntriesAreCheckedAndSkipped(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, before, longLine, status string }{
		{"unmatched prefix", "", strings.Repeat("x", 65537) + "FATAL", "unknown"},
		{"matched prefix", "", "FATAL " + strings.Repeat("x", 65537), "failed"},
		{"match before problem", "FATAL before", strings.Repeat("x", 65537), "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testhelpers.FakeUnit(t, "LoadState=loaded\nActiveState=active\n")
			written := filepath.Join(t.TempDir(), "journal")
			fakeProgram(t, "journalctl", "cat "+testhelpers.ShellQuote(written))
			entry := func(cursor, message string) string {
				data, _ := json.Marshal(map[string]string{"MESSAGE": message, "__CURSOR": cursor, "__REALTIME_TIMESTAMP": "1"})
				return string(data) + "\n"
			}
			os.WriteFile(written, []byte(entry("start", "started")), 0644)
			runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{JournalOfSystemdUnit: "myapp.service", FailWhenANewLineContains: "FATAL"}}}
			if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" {
				t.Fatal(o)
			}
			before := ""
			if tc.before != "" {
				before = entry("before", tc.before)
			}
			os.WriteFile(written, []byte(before+entry("oversized", tc.longLine)), 0644)
			o := runs.run(testhelpers.TestContext(t))
			if o.Status != tc.status {
				t.Fatal(o)
			}
			if tc.name != "matched prefix" && (!strings.Contains(o.Message, "myapp.service") || !strings.Contains(o.Message, "cursor oversized exceeds 64 KiB")) {
				t.Fatal(o)
			}
			if tc.name == "match before problem" && !reflect.DeepEqual(o.Details, []string{"FATAL before"}) {
				t.Fatal(o)
			}
			var read checksandalerts.LogLinesRead
			if err := json.Unmarshal(runs.stored, &read); err != nil {
				t.Fatal(err)
			}
			if read.Cursor != "oversized" {
				t.Fatal(read)
			}
			os.WriteFile(written, []byte(entry("next", "FATAL next")), 0644)
			if o := runs.run(testhelpers.TestContext(t)); o.Status != "failed" || !reflect.DeepEqual(o.Details, []string{"FATAL next"}) {
				t.Fatal(o)
			}
		})
	}
}

func TestLogJournalSilenceUsesTheNewestEntryBeyondABacklog(t *testing.T) {
	t.Parallel()
	testhelpers.FakeUnit(t, "LoadState=loaded\nActiveState=active\n")
	directory := t.TempDir()
	backlog, latest := filepath.Join(directory, "backlog"), filepath.Join(directory, "latest")
	fakeProgram(t, "journalctl", "case \" $* \" in *\" --lines=1 \"*) cat "+testhelpers.ShellQuote(latest)+";; *) cat "+testhelpers.ShellQuote(backlog)+";; esac")
	entry := func(message string, at time.Time) string {
		data, _ := json.Marshal(map[string]string{"MESSAGE": message, "__CURSOR": "entry", "__REALTIME_TIMESTAMP": fmt.Sprint(at.UnixMicro())})
		return string(data) + "\n"
	}
	old := time.Now().Add(-time.Hour)
	if err := os.WriteFile(latest, []byte(entry("last complete line", old)), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(backlog, []byte(strings.Repeat(entry(strings.Repeat("x", 60*1024), old), 150)), 0644); err != nil {
		t.Fatal(err)
	}
	runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{JournalOfSystemdUnit: "myapp.service", FailWhenNoNewLineFor: "10m"}}}
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" {
		t.Fatal(o)
	}
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "failed" || !strings.Contains(o.Message, "since the newest line") {
		t.Fatal(o)
	}
	if err := os.WriteFile(latest, []byte(entry("recent complete line", time.Now())), 0644); err != nil {
		t.Fatal(err)
	}
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" {
		t.Fatal(o)
	}
}

func TestLogJournalReportsMatchesBeforeAReadError(t *testing.T) {
	t.Parallel()
	testhelpers.FakeUnit(t, "LoadState=loaded\nActiveState=active\n")
	directory := t.TempDir()
	written, failure := filepath.Join(directory, "journal"), filepath.Join(directory, "read-error")
	fakeProgram(t, "journalctl", "cat "+testhelpers.ShellQuote(written)+"; if [ -f "+testhelpers.ShellQuote(failure)+" ]; then echo 'journal read error' >&2; exit 1; fi")
	entry := func(cursor, message string) string {
		data, _ := json.Marshal(map[string]string{"MESSAGE": message, "__CURSOR": cursor, "__REALTIME_TIMESTAMP": "1"})
		return string(data) + "\n"
	}
	os.WriteFile(written, []byte(entry("start", "started")), 0644)
	runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{JournalOfSystemdUnit: "myapp.service", FailWhenANewLineContains: "FATAL"}}}
	if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" {
		t.Fatal(o)
	}
	os.WriteFile(written, []byte(entry("matched", "FATAL before error")), 0644)
	os.WriteFile(failure, nil, 0644)
	o := runs.run(testhelpers.TestContext(t))
	if o.Status != "failed" || !strings.Contains(o.Message, "journal read error") || !reflect.DeepEqual(o.Details, []string{"FATAL before error"}) {
		t.Fatal(o)
	}
}

func TestLogSilenceReadsCurrentFileBeforeRotatedBacklog(t *testing.T) {
	t.Parallel()
	for _, current := range []string{"fresh complete line\n", "unfinished line"} {
		t.Run(current, func(t *testing.T) {
			t.Parallel()
			log := logFile{t, filepath.Join(t.TempDir(), "worker.log")}
			log.add("start\n")
			runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: log.path, FailWhenNoNewLineFor: "10m"}}}
			if o := runs.run(context.Background()); o.Status != "ok" {
				t.Fatal(o)
			}
			log.add(strings.Repeat("line\n", 4*1024*1024))
			old := time.Now().Add(-time.Hour)
			if err := os.Chtimes(log.path, old, old); err != nil {
				t.Fatal(err)
			}
			if err := os.Rename(log.path, log.path+".rotated"); err != nil {
				t.Fatal(err)
			}
			log.add(current)
			o := runs.run(context.Background())
			want := "failed"
			if strings.HasSuffix(current, "\n") {
				want = "ok"
			}
			if o.Status != want {
				t.Fatal(o)
			}
			var read checksandalerts.LogLinesRead
			if err := json.Unmarshal(runs.stored, &read); err != nil {
				t.Fatal(err)
			}
			if read.Offset >= int64(len("start\n")+len("line\n")*4*1024*1024) {
				t.Fatal("test did not leave an unread rotated backlog", read)
			}
		})
	}
}

func TestOversizedLogLineSpanningRunsChecksItsPrefixOnce(t *testing.T) {
	t.Parallel()
	log := logFile{t, filepath.Join(t.TempDir(), "application.log")}
	log.add("start\n")
	runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{LogFilePath: log.path, FailWhenANewLineContains: "FATAL"}}}
	if o := runs.run(context.Background()); o.Status != "ok" {
		t.Fatal(o)
	}
	log.add("FATAL prefix " + strings.Repeat("x", 17*1024*1024) + "FATAL beyond the prefix\nFATAL next complete line\n")
	if o := runs.run(context.Background()); o.Status != "failed" || len(o.Details) != 1 || !strings.HasPrefix(o.Details[0], "FATAL prefix ") {
		t.Fatal(o)
	}
	var read checksandalerts.LogLinesRead
	if err := json.Unmarshal(runs.stored, &read); err != nil {
		t.Fatal(err)
	}
	if !read.SkippingOversizedLine {
		t.Fatal("test did not cross a read budget", read)
	}
	// Discarding the remainder survives a restart of the check's in-memory state.
	runs.memory = nil
	if o := runs.run(context.Background()); o.Status != "failed" || !reflect.DeepEqual(o.Details, []string{"FATAL next complete line"}) {
		t.Fatal(o)
	}
	if o := runs.run(context.Background()); o.Status != "ok" {
		t.Fatal(o)
	}
}

func TestLogReadReportsCompleteMatchesBeforeUnexpectedEnd(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "application.log")
	contents := "FATAL before read error\nunfinished"
	if err := os.WriteFile(path, []byte(contents), 0644); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	read := &checksandalerts.LogLinesRead{}
	remaining := len(contents) + 100
	matches := []string{}
	more, problem := checksandalerts.ReadLogLinesForTest(context.Background(), file, read, int64(len(contents)+100), &remaining, false, func(line string) bool { matches = append(matches, line); return true })
	if !more || !strings.Contains(problem, "unexpected EOF") || read.Offset != int64(len("FATAL before read error\n")) || !reflect.DeepEqual(matches, []string{"FATAL before read error"}) {
		t.Fatal(read, problem, matches)
	}
}

func TestLogJournalSkipsAnEntryLargerThanItsBatchBudget(t *testing.T) {
	t.Parallel()
	for _, matches := range []bool{true, false} {
		t.Run(fmt.Sprint(matches), func(t *testing.T) {
			t.Parallel()
			testhelpers.FakeUnit(t, "LoadState=loaded\nActiveState=active\n")
			directory := t.TempDir()
			start, metadata, raw, batch, next := filepath.Join(directory, "start"), filepath.Join(directory, "metadata"), filepath.Join(directory, "raw"), filepath.Join(directory, "batch"), filepath.Join(directory, "next")
			entry := func(cursor, message string) string {
				data, _ := json.Marshal(map[string]string{"MESSAGE": message, "__CURSOR": cursor, "__REALTIME_TIMESTAMP": "1"})
				return string(data) + "\n"
			}
			message := strings.Repeat("x", 9*1024*1024)
			if matches {
				message = "FATAL prefix " + message
			} else {
				message += " FATAL beyond the prefix"
			}
			for path, content := range map[string]string{start: entry("start", "started"), metadata: `{"__CURSOR":"oversized","__REALTIME_TIMESTAMP":"1"}` + "\n", raw: message + "\n", batch: entry("oversized", message), next: entry("next", "FATAL next")} {
				if err := os.WriteFile(path, []byte(content), 0644); err != nil {
					t.Fatal(err)
				}
			}
			fakeProgram(t, "journalctl", "case \" $* \" in *\" --lines=1 \"*) cat "+testhelpers.ShellQuote(start)+";; *\" --output-fields=__CURSOR \"*) cat "+testhelpers.ShellQuote(metadata)+";; *\" --output=cat \"*) cat "+testhelpers.ShellQuote(raw)+";; *\" --after-cursor=oversized \"*) cat "+testhelpers.ShellQuote(next)+";; *) cat "+testhelpers.ShellQuote(batch)+";; esac")
			runs := &scheduledRuns{check: checksandalerts.Check{Inputs: &checksandalerts.LogLines{JournalOfSystemdUnit: "myapp.service", FailWhenANewLineContains: "FATAL"}}}
			if o := runs.run(testhelpers.TestContext(t)); o.Status != "ok" {
				t.Fatal(o)
			}
			o := runs.run(testhelpers.TestContext(t))
			want := "unknown"
			if matches {
				want = "failed"
			}
			if o.Status != want {
				t.Fatal(o)
			}
			if !matches && (!strings.Contains(o.Message, "myapp.service") || !strings.Contains(o.Message, "cursor oversized")) {
				t.Fatal(o)
			}
			if o := runs.run(testhelpers.TestContext(t)); o.Status != "failed" || !reflect.DeepEqual(o.Details, []string{"FATAL next"}) {
				t.Fatal(o)
			}
		})
	}
}
