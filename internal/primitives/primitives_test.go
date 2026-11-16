package primitives_test

import (
	"bufio"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// A file that holds more than its reader allows is refused, whatever it
// holds, with an error that a caller can tell from every other one.
func TestReadFileRefusesAFileBeyondItsLimit(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "file")
	if e := os.WriteFile(path, []byte("12345678"), 0600); e != nil {
		t.Fatal(e)
	}
	if b, e := primitives.ReadFile(path, 8); e != nil || string(b) != "12345678" {
		t.Fatal("a file of exactly its limit:", string(b), e)
	}
	_, e := primitives.ReadFile(path, 7)
	if !errors.Is(e, primitives.ErrFileLargerThanLimit) || !strings.Contains(e.Error(), path+" holds more than 7 bytes") {
		t.Fatal("a file one byte beyond its limit:", e)
	}
	if _, e := primitives.ReadFile(filepath.Join(t.TempDir(), "absent"), 8); e == nil || errors.Is(e, primitives.ErrFileLargerThanLimit) {
		t.Fatal("a file that does not exist:", e)
	}
}

// A program that MMWatchdog runs as itself has MMWatchdog's environment. A program that MMWatchdog
// runs for a check has the check's account, its directory, and only the
// environment that MMWatchdog gives it. An exit code is a result, not an error.
func TestRunCommandRunsAsMMWatchdogItselfOrAsACheck(t *testing.T) {
	t.Setenv("MMWATCHDOG_TEST_INHERITED", "from-the-test")
	script := `printf '%s %s' "$MMWATCHDOG_TEST_INHERITED" "$MMWATCHDOG_TEST_GIVEN"; pwd; printf 'problem' >&2; exit 3`
	own, e := primitives.RunCommand(context.Background(), primitives.Command{Arguments: []string{"/bin/sh", "-c", script}, AsMMWatchdogItself: true})
	if e != nil || own.ExitCode != 3 || !strings.HasPrefix(own.StandardOutput, "from-the-test ") || own.StandardError != "problem" {
		t.Fatalf("as MMWatchdog itself: %+v %v", own, e)
	}
	options := testhelpers.OwnOptions(t)
	options.Env = map[string]string{"MMWATCHDOG_TEST_GIVEN": "from-the-check"}
	directory, _ := filepath.EvalSymlinks(options.RunFromDirectory)
	check, e := primitives.RunCommand(context.Background(), primitives.Command{Arguments: []string{"/bin/sh", "-c", script}, RunAs: &options})
	if e != nil || check.ExitCode != 3 || !strings.HasPrefix(check.StandardOutput, " from-the-check") || check.StandardError != "problem" {
		t.Fatalf("as a check: %+v %v", check, e)
	}
	if printed, _ := filepath.EvalSymlinks(strings.TrimSpace(strings.TrimPrefix(check.StandardOutput, " from-the-check"))); printed != directory {
		t.Fatalf("a check runs in %q, want %q", printed, directory)
	}
	// A check may write 64 KiB to its standard output, and MMWatchdog itself 1 MiB.
	much := primitives.Command{Arguments: []string{"/bin/sh", "-c", "head -c 70000 /dev/zero | tr '\\0' x"}, AsMMWatchdogItself: true}
	if r, e := primitives.RunCommand(context.Background(), much); e != nil || len(r.StandardOutput) != 70000 {
		t.Fatal("70000 bytes as MMWatchdog itself:", len(r.StandardOutput), e)
	}
	much.RunAs, much.AsMMWatchdogItself = &options, false
	if _, e := primitives.RunCommand(context.Background(), much); !errors.Is(e, primitives.ErrStandardOutputLimit) || e.Error() != "check standard output exceeds 65536 bytes" {
		t.Fatal("70000 bytes as a check:", e)
	}
	if _, e := primitives.RunCommand(context.Background(), primitives.Command{Arguments: []string{filepath.Join(t.TempDir(), "no-such-program")}, AsMMWatchdogItself: true}); e == nil {
		t.Fatal("a program that does not exist ran")
	}
	// A caller states one of the two ways. A run that states neither would
	// run as MMWatchdog itself, which is root, by leaving a field out.
	ran := filepath.Join(t.TempDir(), "ran")
	touch := []string{"/bin/sh", "-c", "touch " + testhelpers.ShellQuote(ran)}
	for name, c := range map[string]primitives.Command{"neither way": {Arguments: touch}, "both ways": {Arguments: touch, RunAs: &options, AsMMWatchdogItself: true}} {
		_, e := primitives.RunCommand(context.Background(), c)
		if _, statError := os.Stat(ran); e == nil || statError == nil {
			t.Errorf("a run that states %s ran: %v", name, e)
		}
	}
	// A run that its time limit ends is an error, and its child ends too.
	// The limit passes once the program runs. The shell waits for sleep
	// as for a command of its own: the builtin wait would end with 0 when
	// its child ends first, as a program that did its work.
	started := filepath.Join(t.TempDir(), "started")
	pastItsTimeLimit := []string{"/bin/sh", "-c", "echo $$ > " + testhelpers.ShellQuote(started) + "; sleep 30"}
	for name, tc := range map[string]struct {
		c    primitives.Command
		want string
	}{
		"as a check":           {primitives.Command{Arguments: pastItsTimeLimit, RunAs: &options}, "the run took longer than time_limit_per_run, so MMWatchdog ended it"},
		"as MMWatchdog itself": {primitives.Command{Arguments: pastItsTimeLimit, AsMMWatchdogItself: true}, "/bin/sh did not finish in time: context deadline exceeded"},
	} {
		os.Remove(started)
		limit := testhelpers.NewTimeLimitThatTheTestEnds(context.Background())
		ran := make(chan error, 1)
		go func() { _, e := primitives.RunCommand(limit, tc.c); ran <- e }()
		testhelpers.StartedProgram(t, started)
		ended := time.Now()
		limit.End()
		// A run that the time limit did not end would take 30 seconds.
		if e := <-ran; e == nil || e.Error() != tc.want || time.Since(ended) > 10*time.Second {
			t.Errorf("a run past its time limit %s: %v after %s", name, e, time.Since(ended))
		}
	}
}

// MMWatchdog reads all that a program wrote before its end, however late it begins
// to read: on a busy machine, MMWatchdog can get to the output only after the
// program has ended. A program outside the program's process group can
// hold the output open after that; MMWatchdog does not wait for it.
func TestRunCommandReadsAllThatTheProgramWrote(t *testing.T) {
	t.Parallel()
	ended := filepath.Join(t.TempDir(), "ended")
	readingLate := context.WithValue(context.Background(), primitives.TestBeforeReadingProgramOutputKey{}, func() {
		process := testhelpers.StartedProgram(t, ended)
		testhelpers.WaitFor(t, "the end of the program", func() bool { return syscall.Kill(process, 0) != nil })
		// The outcome is the same however long MMWatchdog is late; a second is
		// more than MMWatchdog waits for an output's end.
		time.Sleep(time.Second)
	})
	script := "printf 'the result'; printf 'the reason' >&2; echo $$ > " + testhelpers.ShellQuote(ended) + "; exit 3"
	r, e := primitives.RunCommand(readingLate, primitives.Command{Arguments: []string{"/bin/sh", "-c", script}, AsMMWatchdogItself: true})
	if e != nil || r.ExitCode != 3 || r.StandardOutput != "the result" || r.StandardError != "the reason" {
		t.Fatalf("reading after the end of the program: %+v %v", r, e)
	}

	// A process outside the program's process group, here the group of the
	// test, keeps a copy of the program's standard output.
	holding := filepath.Join(t.TempDir(), "holding")
	script = "printf 'the result'; sleep 60 & echo $! > " + testhelpers.ShellQuote(holding)
	began := time.Now()
	r, e = primitives.RunCommand(context.Background(), primitives.Command{Arguments: []string{"/bin/sh", "-c", script}, AsMMWatchdogItself: true, StaysInThisProcessGroup: true})
	t.Cleanup(func() { syscall.Kill(testhelpers.StartedProgram(t, holding), syscall.SIGKILL) })
	// Waiting for the end of the output would take a minute.
	if e != nil || r.ExitCode != 0 || r.StandardOutput != "the result" || time.Since(began) > 30*time.Second {
		t.Fatalf("an output that another program holds open: %+v %v after %s", r, e, time.Since(began))
	}
}

// ListDirectory returns the names in a directory, sorted, and ResolvePath
// follows a symbolic link to what it names.
func TestListDirectoryAndResolvePath(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	for _, name := range []string{"b", "a", "c"} {
		if e := os.WriteFile(filepath.Join(directory, name), nil, 0600); e != nil {
			t.Fatal(e)
		}
	}
	if e := os.Mkdir(filepath.Join(directory, "d"), 0700); e != nil {
		t.Fatal(e)
	}
	if names, e := primitives.ListDirectory(directory); e != nil || strings.Join(names, " ") != "a b c d" {
		t.Fatal(names, e)
	}
	if _, e := primitives.ListDirectory(filepath.Join(directory, "missing")); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join(directory, "d"), filepath.Join(directory, "link")); e != nil {
		t.Fatal(e)
	}
	real, e := primitives.ResolvePath(filepath.Join(directory, "d"))
	if e != nil {
		t.Fatal(e)
	}
	if resolved, e := primitives.ResolvePath(filepath.Join(directory, "link")); e != nil || resolved != real {
		t.Fatal(resolved, e)
	}
	if _, e := primitives.ResolvePath(filepath.Join(directory, "missing")); e == nil {
		t.Fatal("resolved a path that does not exist")
	}
}

// systemctl's exit code other than zero is an error that holds what
// systemctl wrote to its standard error, cut to a length that fits a message.
func TestSystemctlReportsWhatSystemctlWrote(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	fake := "#!/bin/sh\nif [ \"$1\" = show ]; then echo ActiveState=active; exit 0; fi\nhead -c 2000 /dev/zero | tr '\\0' e >&2\nexit 1\n"
	testhelpers.WriteStandInProgram(t, filepath.Join(directory, "systemctl"), fake)
	testhelpers.PutProgramsFirst(t, directory)
	if shown, e := primitives.Systemctl(testhelpers.TestContext(t), "show", "--", "myapp.service"); e != nil || shown != "ActiveState=active\n" {
		t.Fatalf("%q %v", shown, e)
	}
	_, e := primitives.Systemctl(testhelpers.TestContext(t), "restart", "--", "myapp.service")
	if e == nil || !strings.HasPrefix(e.Error(), "exit status 1: eeee") || len(e.Error()) > 600 {
		t.Fatal(e)
	}
}

// An HTTP request does not follow a redirect, sends a Host header as the
// name of the virtual host, reads no more than its limit, and never writes
// its address into an error.
func TestHTTPRequestPrimitive(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
		case "/large":
			w.Write([]byte(strings.Repeat("x", primitives.HTTPResponseBodyLimit+1)))
		case "/larger":
			w.Write([]byte(strings.Repeat("x", 2*primitives.HTTPResponseBodyLimit+1)))
		default:
			w.Header().Set("X-Seen-Host", r.Host)
			body := make([]byte, 64)
			n, _ := r.Body.Read(body)
			w.WriteHeader(http.StatusAccepted)
			w.Write([]byte(r.Method + " " + string(body[:n])))
		}
	}))
	defer server.Close()
	ctx := context.Background()
	answer, e := primitives.HTTPRequest(ctx, primitives.HTTPRequestToSend{Method: "POST", URL: server.URL + "/alerts", Headers: map[string]string{"Host": "alerts.example.com"}, Body: []byte("the alert")})
	if e != nil || answer.StatusCode != http.StatusAccepted || string(answer.Body) != "POST the alert" || answer.Headers.Get("X-Seen-Host") != "alerts.example.com" {
		t.Fatalf("%+v %v", answer, e)
	}
	if answer, e := primitives.HTTPRequest(ctx, primitives.HTTPRequestToSend{Method: "GET", URL: server.URL + "/redirect"}); e != nil || answer.StatusCode != http.StatusFound {
		t.Fatalf("a redirect was followed: %+v %v", answer, e)
	}
	if answer, e := primitives.HTTPRequest(ctx, primitives.HTTPRequestToSend{Method: "GET", URL: server.URL + "/large"}); !errors.Is(e, primitives.ErrHTTPResponseBodyLimit) || e.Error() != "the response body exceeds 1 MiB" || answer.StatusCode != http.StatusOK || answer.Body != nil {
		t.Fatalf("a body beyond the limit: %d %v", answer.StatusCode, e)
	}
	// A request's own limit replaces the default, and the error names it.
	twoMiB := 2 * primitives.HTTPResponseBodyLimit
	if answer, e := primitives.HTTPRequest(ctx, primitives.HTTPRequestToSend{Method: "GET", URL: server.URL + "/large", ResponseBodyLimit: twoMiB}); e != nil || len(answer.Body) != primitives.HTTPResponseBodyLimit+1 {
		t.Fatalf("a body within the request's own limit: %d bytes, %v", len(answer.Body), e)
	}
	if answer, e := primitives.HTTPRequest(ctx, primitives.HTTPRequestToSend{Method: "GET", URL: server.URL + "/larger", ResponseBodyLimit: twoMiB}); !errors.Is(e, primitives.ErrHTTPResponseBodyLimit) || e.Error() != "the response body exceeds 2 MiB" || answer.Body != nil {
		t.Fatalf("a body beyond the request's own limit: %d %v", answer.StatusCode, e)
	}
	asked := 0
	unread := primitives.HTTPRequestToSend{Method: "GET", URL: server.URL + "/large", ReadsResponseBody: func(statusCode int) bool { asked = statusCode; return false }}
	if answer, e := primitives.HTTPRequest(ctx, unread); e != nil || answer.StatusCode != http.StatusOK || answer.Body != nil || asked != http.StatusOK {
		t.Fatalf("a body that the caller does not read: %d %v, asked for %d", answer.StatusCode, e, asked)
	}
	// The address of a request can be a secret: an error never holds it.
	closed := testhelpers.AddressWhereNothingListens(t, "tcp")
	secret := "http://" + closed + "/services/T000/B000/SECRETSECRETSECRET"
	for _, address := range []string{secret, "http://alerts.example.com/\x7fSECRETSECRETSECRET", "://SECRETSECRETSECRET"} {
		_, e := primitives.HTTPRequest(ctx, primitives.HTTPRequestToSend{Method: "POST", URL: address})
		if e == nil || strings.Contains(e.Error(), "SECRET") || strings.Contains(e.Error(), closed+"/") {
			t.Errorf("the error of a request to %q: %v", address, e)
		}
	}
}

// A TCP exchange connects, sends its text when it has one, and reads until
// the answer holds the text that the caller waits for.
func TestTCPExchangePrimitive(t *testing.T) {
	t.Parallel()
	testEnds := make(chan struct{})
	t.Cleanup(func() { close(testEnds) })
	listener, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer listener.Close()
	go func() {
		for {
			conn, e := listener.Accept()
			if e != nil {
				return
			}
			go func() {
				defer conn.Close()
				conn.Write([]byte("220 ready\r\n"))
				line, _ := bufio.NewReader(conn).ReadString('\n')
				switch strings.TrimSpace(line) {
				case "PING":
					conn.Write([]byte("+PONG\r\n"))
				case "QUIET":
					<-testEnds
				case "FLOOD":
					conn.Write([]byte(strings.Repeat("x", 2*primitives.TCPExchangeLimit)))
				}
			}()
		}
	}()
	address := listener.Addr().String()
	ctx := context.Background()
	if received, e := primitives.TCPExchange(ctx, address, "", ""); e != nil || received != "" {
		t.Fatalf("a connection alone: %q %v", received, e)
	}
	if received, e := primitives.TCPExchange(ctx, address, "", "\n"); e != nil || received != "220 ready\r\n" {
		t.Fatalf("the greeting: %q %v", received, e)
	}
	if received, e := primitives.TCPExchange(ctx, address, "PING\r\n", "PONG"); e != nil || received != "220 ready\r\n+PONG\r\n" {
		t.Fatalf("a text and its answer: %q %v", received, e)
	}
	// The other side closes without the awaited text: the caller gets what
	// came.
	if received, e := primitives.TCPExchange(ctx, address, "BYE\r\n", "never sent"); e != nil || received != "220 ready\r\n" {
		t.Fatalf("an answer without the awaited text: %q %v", received, e)
	}
	// The exchange reads no more than its limit.
	if received, e := primitives.TCPExchange(ctx, address, "FLOOD\r\n", "never sent"); e != nil || len(received) != primitives.TCPExchangeLimit {
		t.Fatalf("an answer beyond the limit: %d bytes %v", len(received), e)
	}
	// The server says nothing more, and the time limit ends the exchange,
	// which would otherwise wait until the test ends.
	limited, cancel := context.WithTimeout(ctx, testhelpers.TimeLimitThatATestLetsPass)
	defer cancel()
	start := time.Now()
	if received, e := primitives.TCPExchange(limited, address, "QUIET\r\n", "never sent"); !errors.Is(e, context.DeadlineExceeded) || received != "220 ready\r\n" || time.Since(start) > 30*time.Second {
		t.Fatalf("an exchange past its time limit: %q %v after %s", received, e, time.Since(start))
	}
	if _, e := primitives.TCPExchange(ctx, testhelpers.AddressWhereNothingListens(t, "tcp"), "", ""); e == nil {
		t.Fatal("a closed port accepted a connection")
	}
}

// A program that ended by itself did its work, also when something that it
// started still runs and holds its output open. MMWatchdog does not wait for that,
// and ends what the program left running in its process group.
func TestAProgramThatLeavesSomethingRunningEndedByItself(t *testing.T) {
	t.Parallel()
	processFile := filepath.Join(t.TempDir(), "process")
	start := time.Now()
	ran, e := primitives.RunCommand(context.Background(), primitives.Command{Arguments: []string{"sh", "-c", "sleep 30 & echo $! > " + processFile + "; echo started"}, AsMMWatchdogItself: true})
	if e != nil || ran.ExitCode != 0 || ran.StandardOutput != "started\n" || time.Since(start) > 10*time.Second {
		t.Fatalf("after %s: %+v %v", time.Since(start), ran, e)
	}
	written, e := os.ReadFile(processFile)
	process, notANumber := strconv.Atoi(strings.TrimSpace(string(written)))
	if e != nil || notANumber != nil {
		t.Fatal(e, notANumber)
	}
	for end := time.Now().Add(testhelpers.LongestWaitForAnEvent); syscall.Kill(process, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(end) {
			syscall.Kill(process, syscall.SIGKILL)
			t.Fatalf("what the program left running, process %d, still runs", process)
		}
	}
}
