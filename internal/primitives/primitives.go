// Package primitives holds the functions through which every module reads
// the node: a file, a program, an HTTP request, a TCP connection
// (docs/changing-mmwatchdog/design/checks.md, "Primitives"), with the processes of the node,
// the questions MMWatchdog asks systemd, and the family of the node's distribution.
// A module touches the node in no other way, so this package is the one
// place to review how MMWatchdog does it. It knows nothing of checks, incidents, or
// alerts.
package primitives

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/dns/dnsmessage"
)

// ErrFileLargerThanLimit reports a file that holds more than its reader
// allows.
var ErrFileLargerThanLimit = errors.New("the file is larger than its limit")

// ReadFile reads the file at path, which holds at most limit bytes. A larger
// file is ErrFileLargerThanLimit, so a caller can say what the file should
// hold.
func ReadFile(path string, limit int) ([]byte, error) {
	f, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = f.Close() // Closing this read-only handle has no pending writes to commit.
	}()
	b, e := io.ReadAll(io.LimitReader(f, int64(limit)+1))
	if e != nil {
		return nil, e
	}
	if len(b) > limit {
		return nil, fmt.Errorf("%s holds more than %d bytes: %w", path, limit, ErrFileLargerThanLimit)
	}
	return b, nil
}

// ProcDirectory and SysDirectory are the two directories in which the Linux
// kernel shows its state as files. A test puts a directory of its own in
// their place.
var ProcDirectory, SysDirectory = "/proc", "/sys"

// The accounts of the operating system as which a core module runs a
// program. UnprivilegedAccount is the account of MMWatchdog's checks, which can
// change nothing on the node. The server's own administrator is the account
// postgres for PostgreSQL and root for MySQL and MariaDB: each server takes
// that account, through its local socket, without a password. A test puts
// its own account in their place.
var UnprivilegedAccount, PostgreSQLAdministratorAccount, MySQLAdministratorAccount = "mmwatchdog", "postgres", "root"

// NodeFilesRoot stands in front of the path of a file that says something
// about the node's operating system, such as /etc/os-release or
// /run/reboot-required. It is empty, and a test puts a directory of its own
// there.
var NodeFilesRoot = ""

// NodeFile returns where MMWatchdog reads the file of the operating system that has
// the absolute path path.
func NodeFile(path string) string { return NodeFilesRoot + path }

// directoryEntriesLimit is the most names that ListDirectory returns.
const directoryEntriesLimit = 100000

// ListDirectory returns the names of the files and directories in the
// directory at path, sorted, at most directoryEntriesLimit of them.
func ListDirectory(path string) ([]string, error) {
	directory, e := os.Open(path)
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = directory.Close() // Closing this read-only handle has no pending writes to commit.
	}()
	names, e := directory.Readdirnames(directoryEntriesLimit)
	if e != nil && e != io.EOF {
		return nil, e
	}
	slices.Sort(names)
	return names, nil
}

// ResolvePath returns the path that path names once every symbolic link in
// it is followed, such as /dev/dm-0 for /dev/mapper/data.
func ResolvePath(path string) (string, error) { return filepath.EvalSymlinks(path) }

// MountedFilesystem is one filesystem that is mounted on the node, as the
// kernel lists it.
type MountedFilesystem struct {
	// MountPoint is the path where the filesystem is mounted.
	MountPoint string
	// FilesystemType is the kind of filesystem, such as ext4, nfs4, or
	// tmpfs, and source is what is mounted: a disk device such as
	// /dev/sda1, an export of another machine, or a name of the kernel's.
	FilesystemType, Source string
	// DeviceNumbers are the kernel's numbers for the filesystem, such as
	// 8:1. Two mounts of one filesystem have the same.
	DeviceNumbers string
}

// MountedFilesystems returns every filesystem that is mounted on the node,
// in the order in which the kernel lists them.
func MountedFilesystems() ([]MountedFilesystem, error) {
	data, e := ReadFile(ProcDirectory+"/self/mountinfo", ProcFileLimit)
	if e != nil {
		return nil, e
	}
	return ParseMountInformation(string(data)), nil
}

// ParseMountInformation reads the text of /proc/self/mountinfo. A line
// holds the mount's numbers, the device numbers, the mounted directory of
// the filesystem, the mount point, and the mount options; then optional
// fields up to a hyphen; then the filesystem type and the source. A line
// that has another form is left out.
func ParseMountInformation(data string) []MountedFilesystem {
	mounted := []MountedFilesystem{}
	for _, line := range strings.Split(data, "\n") {
		fields := strings.Fields(line)
		hyphen := slices.Index(fields, "-")
		if len(fields) < 7 || hyphen < 6 || hyphen+2 >= len(fields) {
			continue
		}
		mounted = append(mounted, MountedFilesystem{MountPoint: UnescapeMountField(fields[4]), FilesystemType: fields[hyphen+1],
			Source: UnescapeMountField(fields[hyphen+2]), DeviceNumbers: fields[2]})
	}
	return mounted
}

// UnescapeMountField puts back the characters that the kernel writes as a
// backslash and three octal digits in its list of mounts: a space is \040.
func UnescapeMountField(field string) string {
	if !strings.Contains(field, "\\") {
		return field
	}
	text := []byte{}
	for i := 0; i < len(field); i++ {
		if field[i] == '\\' && i+3 < len(field) {
			if code, e := strconv.ParseUint(field[i+1:i+4], 8, 8); e == nil {
				text = append(text, byte(code))
				i += 3
				continue
			}
		}
		text = append(text, field[i])
	}
	return string(text)
}

// ErrFilesystemDoesNotAnswer reports a filesystem about which the kernel
// gave no answer in time, as a filesystem on another machine does when that
// machine is gone.
var ErrFilesystemDoesNotAnswer = errors.New("the filesystem does not answer")

// FilesystemQuestion is one question to the kernel about the filesystem at
// a path. answered is closed when counts and err hold the kernel's answer.
type FilesystemQuestion struct {
	asked    time.Time
	Answered chan struct{}
	counts   FilesystemCounts
	err      error
}

// FilesystemQuestions holds, by path, the one question about the filesystem
// there that waits for the kernel's answer.
var FilesystemQuestions = struct {
	sync.Mutex
	Waiting map[string]*FilesystemQuestion
}{Waiting: map[string]*FilesystemQuestion{}}

// AskKernelAboutFilesystem is statFilesystem. A test puts a kernel of its
// own in its place.
var AskKernelAboutFilesystem = statFilesystem

// FilesystemStatistics asks the kernel for the space and the inodes of the
// filesystem that holds path, and waits for the answer no longer than ctx
// allows. The kernel can hold the question about a filesystem on another
// machine without end, and nothing can take the question back. So MMWatchdog keeps
// at most one question about a path waiting: a caller that finds one waiting
// asks no second one, and waits for that same answer. Two checks of one
// filesystem that run in the same moment both get the answer, and a check
// that runs every minute against a filesystem that does not answer keeps
// one question waiting, and not one more for each run. A caller whose time
// ends without an answer gets ErrFilesystemDoesNotAnswer.
func FilesystemStatistics(ctx context.Context, path string) (FilesystemCounts, error) {
	questions := &FilesystemQuestions
	questions.Lock()
	question, waiting := questions.Waiting[path]
	if !waiting {
		question = &FilesystemQuestion{asked: time.Now(), Answered: make(chan struct{})}
		questions.Waiting[path] = question
		go func() {
			question.counts, question.err = AskKernelAboutFilesystem(path)
			questions.Lock()
			delete(questions.Waiting, path)
			questions.Unlock()
			close(question.Answered)
		}()
	}
	questions.Unlock()
	select {
	case <-question.Answered:
		return question.counts, question.err
	case <-ctx.Done():
		// An answer that came in the same moment counts.
		select {
		case <-question.Answered:
			return question.counts, question.err
		default:
		}
		if waiting {
			return FilesystemCounts{}, fmt.Errorf("%w: MMWatchdog has waited for the filesystem at %s since %s", ErrFilesystemDoesNotAnswer, path, question.asked.UTC().Format("15:04:05 UTC"))
		}
		return FilesystemCounts{}, fmt.Errorf("%w: the filesystem at %s gave no answer within the time limit of the run", ErrFilesystemDoesNotAnswer, path)
	}
}

// IsExecutableFile reports a regular file that an account may run.
func IsExecutableFile(path string) bool {
	info, e := os.Stat(path)
	return e == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 //nolint:nilaway // os.Stat returns a nonnil FileInfo on success; the short-circuit error guard runs before its methods.
}

// IsDirectory reports a directory.
func IsDirectory(path string) bool {
	info, e := os.Stat(path)
	return e == nil && info.IsDir() //nolint:nilaway // os.Stat returns a nonnil FileInfo on success; the short-circuit error guard runs before its methods.
}

// LookUpUserAccount finds a user account of the node by its name. A test
// puts accounts of its own in its place, and so learns as which account MMWatchdog
// runs a program.
var LookUpUserAccount = user.Lookup

// OwnStandardOutputLimit is the most that a program MMWatchdog runs as itself, such
// as systemctl, may write to its standard output.
const OwnStandardOutputLimit = 1024 * 1024

// Command is one program for RunCommand to run. It states one of runAs and
// asMMWatchdogItself: no caller runs a program as root by leaving a field out.
type Command struct {
	Arguments     []string
	StandardInput []byte
	// RunAs says under which account, in which directory, and with which
	// environment the program runs: the way of a shell check and of a
	// module that is not core.
	RunAs *ExecOptions
	// AsMMWatchdogItself runs the program as this process, in its directory and
	// with its environment. In the daemon that is root: the way MMWatchdog asks
	// systemd. In the process that runs a Starlark script it is the
	// module's account: the way a script runs a program.
	AsMMWatchdogItself bool
	// StandardOutputLimit is the most that MMWatchdog reads of the program's
	// standard output. 0 is the limit of the way the program runs:
	// ResultLimit with runAs, and OwnStandardOutputLimit as MMWatchdog itself.
	StandardOutputLimit int
	// EndsAtStandardOutputLimit ends the program once it has written more
	// than the limit: the way to read the start of an output that can be
	// very long, such as the journal of a unit. Without it the program runs
	// to its end, as a program must that does more than write.
	EndsAtStandardOutputLimit bool
	// StaysInThisProcessGroup starts the program in the process group of
	// this process and not in one of its own. The process that runs a
	// Starlark script states it: the daemon ends that process's whole
	// process group at the time limit, and so ends every program that the
	// script started. Without it, MMWatchdog ends the program's own process group.
	StaysInThisProcessGroup bool
}

// CommandResult is what a program that ran to its end left behind.
type CommandResult struct {
	ExitCode                      int
	StandardOutput, StandardError string
}

// ErrStandardOutputLimit reports a program that wrote more to its standard
// output than MMWatchdog reads, which a check treats as an execution error. The
// error of a run says how many bytes MMWatchdog reads.
var ErrStandardOutputLimit = errors.New("check standard output exceeds")

// ErrTerminatedBySignal reports a process that a signal ended.
var ErrTerminatedBySignal = errors.New("check process was terminated by a signal")

type timeLimitPerRunKey struct{}

// WithTimeLimitPerRun states the check's time_limit_per_run on ctx, so that
// the error of a program that the limit ended can name the limit.
func WithTimeLimitPerRun(ctx context.Context, limit time.Duration) context.Context {
	return context.WithValue(ctx, timeLimitPerRunKey{}, limit)
}

// TimeLimitEndedRunError is the error of a program of a check that ran into
// the end of its run's time limit. It unwraps to the error of ctx, so
// errors.Is(e, context.DeadlineExceeded) still holds. The program has no
// exit code: MMWatchdog ended it.
type TimeLimitEndedRunError struct {
	limit time.Duration // 0 when ctx does not state it
	cause error
}

func (e *TimeLimitEndedRunError) Error() string {
	limit := "time_limit_per_run"
	if e.limit > 0 {
		limit = e.limit.String() + " (time_limit_per_run)"
	}
	return "the run took longer than " + limit + ", so MMWatchdog ended it"
}

func (e *TimeLimitEndedRunError) Unwrap() error { return e.cause }

// RunCommand runs one program to its end and returns its exit code and what
// it wrote. It runs no shell. A program that ended with an exit code, zero
// or not, is no error. These are errors, each with what the program wrote
// until then: a program that cannot start, a run that ctx ended, an end by
// a signal, and more standard output than the limit. The program and
// everything that it started end with the run.
func RunCommand(ctx context.Context, c Command) (CommandResult, error) {
	failed := func(e error) (CommandResult, error) { return CommandResult{ExitCode: -1}, e }
	if (c.RunAs != nil) == c.AsMMWatchdogItself {
		return failed(errors.New("a program run must select exactly one account: the check's user account or MMWatchdog's own account"))
	}
	cmd := exec.CommandContext(ctx, c.Arguments[0], c.Arguments[1:]...)
	if replaced := testProgramEnvironment(ctx); len(replaced) > 0 {
		cmd.Path, cmd.Err = lookPathOfTest(replaced, c.Arguments[0])
		if c.AsMMWatchdogItself {
			// exec.Cmd uses the last value of a name that appears twice.
			cmd.Env = os.Environ()
			for _, name := range slices.Sorted(maps.Keys(replaced)) {
				cmd.Env = append(cmd.Env, name+"="+replaced[name])
			}
		}
	}
	cmd.Stdin = bytes.NewReader(c.StandardInput)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: !c.StaysInThisProcessGroup}
	standardOutputLimit := OwnStandardOutputLimit
	if c.RunAs != nil {
		standardOutputLimit = ResultLimit
	}
	if c.StandardOutputLimit > 0 {
		standardOutputLimit = c.StandardOutputLimit
	}
	if opts := c.RunAs; opts != nil {
		u, err := LookUpUserAccount(opts.RunAsUserAccount)
		if err != nil {
			return failed(fmt.Errorf("run_as_user_account %q: %w", opts.RunAsUserAccount, err))
		}
		uid, err := ParseID(u.Uid)
		if err != nil {
			return failed(err)
		}
		gid, err := ParseID(u.Gid)
		if err != nil {
			return failed(err)
		}
		cmd.Dir = opts.RunFromDirectory
		cmd.Env = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "HOME=" + u.HomeDir, "USER=" + u.Username, "LANG=C.UTF-8"}
		for _, k := range slices.Sorted(maps.Keys(opts.Env)) {
			cmd.Env = append(cmd.Env, k+"="+opts.Env[k])
		}
		if uint32(os.Geteuid()) != uid || uint32(os.Getegid()) != gid {
			if os.Geteuid() != 0 {
				return failed(fmt.Errorf("running as the user account %s requires root", opts.RunAsUserAccount))
			}
			groups, e := u.GroupIds()
			if e != nil {
				return failed(e)
			}
			gids := []uint32{}
			for _, g := range groups {
				v, e := ParseID(g)
				if e != nil {
					return failed(e)
				}
				gids = append(gids, v)
			}
			cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid, Groups: gids}
		}
	}
	stdout := &limitedBuffer{limit: standardOutputLimit}
	stderr := &limitedBuffer{limit: DiagnosticLimit}
	standardOutput, err := newProgramOutput(stdout)
	if err != nil {
		return failed(err)
	}
	defer standardOutput.close()
	standardError, err := newProgramOutput(stderr)
	if err != nil {
		return failed(err)
	}
	defer standardError.close()
	cmd.Stdout, cmd.Stderr = standardOutput.write, standardError.write
	end := func() error {
		if cmd.Process == nil {
			return os.ErrProcessDone
		}
		if c.StaysInThisProcessGroup {
			return cmd.Process.Kill()
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.Cancel = end
	if c.EndsAtStandardOutputLimit {
		stdout.whenOver = sync.OnceFunc(func() { _ = end() })
	}
	if err := cmd.Start(); err != nil {
		return failed(err)
	}
	// Only the program writes to its outputs: without this process's
	// copy of each pipe's end, a pipe ends when the program's copies do.
	_ = standardOutput.write.Close() // Closing this pipe endpoint releases resources and does not flush buffered file data.
	_ = standardError.write.Close()  // Closing this pipe endpoint releases resources and does not flush buffered file data.
	go standardOutput.readUntilItEnds(ctx)
	go standardError.readUntilItEnds(ctx)
	err = cmd.Wait()
	// Whatever the program left running in its process group ends with it.
	// A program that left the group, as a daemon does when it starts, is
	// out of MMWatchdog's reach.
	if !c.StaysInThisProcessGroup {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	readEverythingThatTheProgramWrote(standardOutput, standardError)
	r := CommandResult{cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()}
	if stderr.overflow {
		r.StandardError += "\n[stderr truncated]"
	}
	// A program that MMWatchdog ended at the limit of its standard output wrote
	// more than the limit, whatever else is true of its end.
	if stdout.overflow && c.EndsAtStandardOutputLimit {
		return r, fmt.Errorf("%w %d bytes", ErrStandardOutputLimit, standardOutputLimit)
	}
	// A program of a check that ran into the end of ctx is a run that did
	// not finish, whatever its exit code. A program that MMWatchdog ran as itself
	// and that ended by itself in that same moment did its work.
	if ctx.Err() != nil && c.RunAs != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			limit, _ := ctx.Value(timeLimitPerRunKey{}).(time.Duration)
			return r, &TimeLimitEndedRunError{limit, ctx.Err()}
		}
		return r, fmt.Errorf("check execution: %w", ctx.Err())
	}
	if ctx.Err() != nil && r.ExitCode != 0 {
		return r, fmt.Errorf("%s did not finish in time: %w", c.Arguments[0], ctx.Err())
	}
	if r.ExitCode < 0 {
		return r, ErrTerminatedBySignal
	}
	if stdout.overflow {
		return r, fmt.Errorf("%w %d bytes", ErrStandardOutputLimit, standardOutputLimit)
	}
	// A program that ended by itself did its work, also in the moment that
	// ctx ended; exec.Cmd then reports the end of ctx. A program that MMWatchdog
	// ended has no exit code of 0: MMWatchdog ends programs with SIGKILL.
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) && (ctx.Err() == nil || !errors.Is(err, ctx.Err())) {
		return r, err
	}
	return r, nil
}

// programOutput is a pipe through which MMWatchdog reads one output of a program
// that it runs, the standard output or the standard error, into to. MMWatchdog
// reads through pipes of its own and not through those of exec.Cmd:
// exec.Cmd stops reading its pipes a fixed time after the program ends,
// and on a busy machine what the program wrote before its end can still
// be in a pipe then. MMWatchdog would lose it, and with it the result of a check
// or the reason that a program gives for its failure.
type programOutput struct {
	read, write *os.File
	to          io.Writer
	ended       chan struct{}
}

func newProgramOutput(to io.Writer) (*programOutput, error) {
	read, write, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &programOutput{read: read, write: write, to: to, ended: make(chan struct{})}, nil
}

// close closes what is still open of the pipe.
func (o *programOutput) close() {
	_ = o.read.Close()  // Closing this pipe endpoint releases resources and does not flush buffered file data.
	_ = o.write.Close() // Closing this pipe endpoint releases resources and does not flush buffered file data.
}

// mostThatAPipeHolds is the largest pipe that a program without the
// rights of root can make on Linux (/proc/sys/fs/pipe-max-size), and so
// the most that a program can leave unread in a pipe at its end.
const mostThatAPipeHolds = 1024 * 1024

// readUntilItEnds reads the output until its end, or until
// readEverythingThatTheProgramWrote stops waiting for its end.
func (o *programOutput) readUntilItEnds(ctx context.Context) {
	defer close(o.ended)
	if beforeReading, ok := ctx.Value(TestBeforeReadingProgramOutputKey{}).(func()); ok {
		beforeReading()
	}
	_, err := io.Copy(o.to, o.read)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		return
	}
	// MMWatchdog stopped waiting for the end of the output, which a program outside
	// the program's process group still holds open. What the program wrote
	// before its end is in the pipe: read what is there now, and wait for
	// nothing more.
	_ = o.read.SetReadDeadline(time.Time{}) // This live pipe already delivered a deadline error, establishing deadline support, and remains open until reading ends.
	pipe, err := o.read.SyscallConn()
	if err != nil {
		return
	}
	_ = pipe.Read(func(descriptor uintptr) bool { // The owned pipe remains open throughout this callback, which always finishes without requesting a poll wait.
		buffer := make([]byte, mostThatAPipeHolds)
		for {
			// One read takes all that is in a pipe, up to the buffer's size.
			n, err := syscall.Read(int(descriptor), buffer)
			if err == syscall.EINTR {
				continue
			}
			if n > 0 {
				_, _ = o.to.Write(buffer[:n]) // Both actual destinations are limitedBuffer values whose Write always returns the full count and nil.
			}
			return true
		}
	})
}

// waitForOutputsHeldByOtherPrograms is how long MMWatchdog waits for the end of a
// program's outputs once the program and its process group have ended.
// Then only a program outside that group can still hold an output open,
// such as a daemon that the program started, and MMWatchdog does not wait for it.
// The time is not a limit on what MMWatchdog reads: readUntilItEnds reads what the
// program wrote, however late it gets to.
const waitForOutputsHeldByOtherPrograms = 200 * time.Millisecond

// readEverythingThatTheProgramWrote returns once each output has ended, or
// once MMWatchdog has stopped waiting for its end and has read what is in it.
func readEverythingThatTheProgramWrote(outputs ...*programOutput) {
	stopWaiting := time.NewTimer(waitForOutputsHeldByOtherPrograms)
	defer stopWaiting.Stop()
	for _, output := range outputs {
		select {
		case <-output.ended:
			continue
		case <-stopWaiting.C:
		}
		for _, output := range outputs {
			_ = output.read.SetReadDeadline(time.Now()) // If the pipe cannot end, the daemon’s waitForRun reports runThatDoesNotEnd and prevents overlapping runs.
		}
		break
	}
	for _, output := range outputs {
		<-output.ended
	}
}

// TestBeforeReadingProgramOutputKey is the key of a context value that only
// tests set: a function that RunCommand calls before it reads an output of
// the program. A test makes MMWatchdog begin to read late this way, as on a busy
// machine. In MMWatchdog itself no context carries the value.
type TestBeforeReadingProgramOutputKey struct{}

// TestProgramEnvironmentKey is the key of a context value that only tests
// set: a function that returns environment variables which replace MMWatchdog's own
// for the programs that MMWatchdog starts under that context, as the variables are
// when each program starts. Their PATH is also where MMWatchdog finds a program by
// its name, whether the program runs as MMWatchdog itself or as a check's account.
// A test gives MMWatchdog stand-ins for a node's programs, such as systemctl, this
// way, without changing the environment of the whole test process, so that
// the test can run beside other tests. In MMWatchdog itself no context carries the
// value, and programs get MMWatchdog's own environment and PATH.
type TestProgramEnvironmentKey struct{}

// testProgramEnvironment is what a test has set under ctx in place of MMWatchdog's
// own environment variables; nil in MMWatchdog itself.
func testProgramEnvironment(ctx context.Context) map[string]string {
	if variables, ok := ctx.Value(TestProgramEnvironmentKey{}).(func() map[string]string); ok {
		return variables()
	}
	return nil
}

// lookPathOfTest finds a program as exec.LookPath does, on the PATH among
// the variables that a test set (testProgramEnvironment), or on MMWatchdog's own
// PATH when the test set none.
func lookPathOfTest(variables map[string]string, name string) (string, error) {
	path, replacedPath := variables["PATH"]
	if !replacedPath || strings.Contains(name, "/") {
		return exec.LookPath(name)
	}
	for _, directory := range filepath.SplitList(path) {
		if !filepath.IsAbs(directory) {
			continue
		}
		if program, err := exec.LookPath(filepath.Join(directory, name)); err == nil {
			return program, nil
		}
	}
	return "", &exec.Error{Name: name, Err: exec.ErrNotFound}
}

// ProgramIsLinkTo reports whether the program that name finds on the PATH
// is a symbolic link, or a chain of links, through a file called target: on
// Fedora 41 and newer, dnf is a link to dnf5. Reading the links costs no
// process. A program that cannot be found, or whose links cannot be read, is
// no link to target.
func ProgramIsLinkTo(ctx context.Context, name, target string) bool {
	var path string
	var err error
	if replaced := testProgramEnvironment(ctx); len(replaced) > 0 {
		path, err = lookPathOfTest(replaced, name)
	} else {
		path, err = exec.LookPath(name)
	}
	// A chain of more than 10 links is a loop or not a program's link.
	for range 10 {
		if err != nil {
			return false
		}
		var next string
		if next, err = os.Readlink(path); err != nil {
			return false
		}
		if !filepath.IsAbs(next) {
			next = filepath.Join(filepath.Dir(path), next)
		}
		if filepath.Base(next) == target {
			return true
		}
		path = next
	}
	return false
}

// Bash runs a script noninteractively with pipefail, without profile or rc
// files.
func Bash(script string) []string {
	return []string{"/bin/bash", "--noprofile", "--norc", "-o", "pipefail", "-c", script}
}

// RunSystemProgram runs a program through which MMWatchdog asks the system, such
// as systemctl or journalctl, as MMWatchdog itself, and returns what the program
// wrote to its standard output. An exit code other than zero is an error
// that holds what the program wrote to its standard error.
func RunSystemProgram(ctx context.Context, arguments ...string) (string, error) {
	r, e := RunCommand(ctx, Command{Arguments: arguments, AsMMWatchdogItself: true})
	if e != nil {
		return r.StandardOutput, e
	}
	if r.ExitCode != 0 {
		problem := fmt.Sprintf("exit status %d", r.ExitCode)
		if said := strings.TrimSpace(r.StandardError); said != "" {
			problem += ": " + Clip(said, 512)
		}
		return r.StandardOutput, errors.New(problem)
	}
	return r.StandardOutput, nil
}

// Systemctl asks systemd through its systemctl program.
func Systemctl(ctx context.Context, arguments ...string) (string, error) {
	return RunSystemProgram(ctx, append([]string{"systemctl"}, arguments...)...)
}

// HTTPResponseBodyLimit is the most that HTTPRequest reads of a response
// when the request sets no ResponseBodyLimit.
const HTTPResponseBodyLimit = 1024 * 1024

// ErrHTTPResponseBodyLimit reports a response that is larger than
// HTTPRequest reads. The error that HTTPRequest returns names the limit.
var ErrHTTPResponseBodyLimit = errors.New("the response body exceeds the limit")

// responseBodyTooLarge is ErrHTTPResponseBodyLimit with the limit that its
// request used.
type responseBodyTooLarge struct{ limit int }

func (e responseBodyTooLarge) Error() string {
	return "the response body exceeds " + strconv.FormatFloat(float64(e.limit)/(1024*1024), 'f', -1, 64) + " MiB"
}

func (e responseBodyTooLarge) Is(target error) bool { return target == ErrHTTPResponseBodyLimit }

// HTTPRequestToSend is one request for HTTPRequest.
type HTTPRequestToSend struct {
	Method, URL string
	// Headers are sent as written. A Host header selects the virtual host
	// and, for HTTPS, the name that is sent and verified.
	Headers map[string]string
	Body    []byte
	// CertificateAuthorities replace the system trust store when they are
	// not nil.
	CertificateAuthorities                *x509.CertPool
	DangerouslySkipServerCertificateCheck bool
	// ReadsResponseBody says, for the status code of the answer, whether
	// the caller needs the response body. nil reads every body. A caller
	// that needs no body, or the body of some answers only, does not wait
	// for a body that is slow or has no end.
	ReadsResponseBody func(statusCode int) bool
	// ResponseBodyLimit is the most bytes of a response body that
	// HTTPRequest reads. Zero reads up to HTTPResponseBodyLimit.
	ResponseBodyLimit int
}

// httpResponse is what the server answered.
type httpResponse struct {
	StatusCode int
	Headers    http.Header
	Body       []byte
}

// HTTPRequest sends one HTTP or HTTPS request and returns the answer. It
// does not follow a redirect, and verifies an HTTPS certificate unless
// explicitly disabled.
// Its errors never hold the request's address, which can be a secret, such
// as the address of a chat webhook: a caller whose address is no secret
// adds it. A response body beyond the request's limit is
// ErrHTTPResponseBodyLimit, returned with the status code and the headers.
func HTTPRequest(ctx context.Context, r HTTPRequestToSend) (httpResponse, error) {
	var toSend io.Reader
	if len(r.Body) > 0 {
		toSend = bytes.NewReader(r.Body)
	}
	req, err := http.NewRequestWithContext(ctx, r.Method, r.URL, toSend)
	if err != nil {
		return httpResponse{}, withoutAddress(err)
	}
	tlsConfig := ServerTLSConfig(r.CertificateAuthorities, r.DangerouslySkipServerCertificateCheck)
	for name, value := range r.Headers {
		if http.CanonicalHeaderKey(name) != "Host" {
			req.Header.Set(name, value)
			continue
		}
		req.Host, tlsConfig.ServerName = value, value
		if host, _, err := net.SplitHostPort(value); err == nil {
			tlsConfig.ServerName = host
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig
	transport.TLSHandshakeTimeout = 0
	transport.DialContext = (&net.Dialer{}).DialContext
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return httpResponse{}, withoutAddress(err)
	}
	defer resp.Body.Close()
	answer := httpResponse{StatusCode: resp.StatusCode, Headers: resp.Header}
	if r.ReadsResponseBody != nil && !r.ReadsResponseBody(resp.StatusCode) {
		return answer, nil
	}
	limit := r.ResponseBodyLimit
	if limit == 0 {
		limit = HTTPResponseBodyLimit
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
	if err != nil {
		return answer, withoutAddress(err)
	}
	if len(body) > limit {
		return answer, responseBodyTooLarge{limit}
	}
	answer.Body = body
	return answer, nil
}

// withoutAddress returns the cause of an error of an HTTP request, without
// the request's method and address that Go writes in front of it.
func withoutAddress(err error) error {
	var withAddress *url.Error
	if errors.As(err, &withAddress) {
		return withAddress.Err
	}
	return err
}

// ConnectTCP opens a TCP connection to address, written as host:port, for a
// core module that speaks a protocol of its own on it, such as SMTP.
func ConnectTCP(ctx context.Context, address string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "tcp", address)
}

// ConnectUnixSocket opens a connection to the Unix socket at path: a file
// through which programs on the same machine talk to each other.
func ConnectUnixSocket(ctx context.Context, path string) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unix", path)
}

// ResolveHostName returns the addresses of a host, in the order in which
// the node's own resolver gives them. An address is its own answer.
func ResolveHostName(ctx context.Context, host string) ([]netip.Addr, error) {
	if address, e := netip.ParseAddr(host); e == nil {
		return []netip.Addr{address}, nil
	}
	addresses, e := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	for i := range addresses {
		addresses[i] = addresses[i].Unmap()
	}
	return addresses, e
}

// PingRequests is how many requests one ping sends at most.
const PingRequests = 3

// ErrPingNeedsRoot reports that this process may not send a ping request:
// only root may open the socket that MMWatchdog uses for one.
var ErrPingNeedsRoot = errors.New("MMWatchdog needs the rights of root to send a ping request")

// ErrNoPingAnswer reports a machine that answered none of the requests.
var ErrNoPingAnswer = errors.New("no answer to a ping request")

// Ping asks the machine at address to answer, with the kind of request that
// the ping program sends: an ICMP echo request. It sends up to PingRequests
// requests, at most a second apart, and returns the time between the first
// request that got an answer and that answer. ctx ends the wait for the
// answer to the last request. Without an answer the error is
// ErrNoPingAnswer.
func Ping(ctx context.Context, address netip.Addr) (time.Duration, error) {
	network, anyAddress, requestType, replyType := "ip4:icmp", "0.0.0.0", byte(8), byte(0)
	if address.Is6() {
		network, anyAddress, requestType, replyType = "ip6:ipv6-icmp", "::", byte(128), byte(129)
	}
	conn, e := net.ListenPacket(network, anyAddress)
	if e != nil {
		if errors.Is(e, os.ErrPermission) {
			return 0, ErrPingNeedsRoot
		}
		return 0, e
	}
	defer conn.Close()
	// The socket receives every ICMP message that reaches the node. The
	// answer to this ping is the one that carries this random marker.
	marker := make([]byte, 10)
	if _, e := rand.Read(marker); e != nil {
		return 0, e
	}
	interval := time.Second
	deadline, limited := ctx.Deadline()
	if limited {
		interval = min(interval, time.Until(deadline)/PingRequests)
	}
	target := &net.IPAddr{IP: address.AsSlice(), Zone: address.Zone()}
	sent := [PingRequests]time.Time{}
	received := make([]byte, 1500)
	for sequence := range PingRequests {
		sent[sequence] = time.Now()
		if _, e := conn.WriteTo(EchoRequest(requestType, marker, sequence), target); e != nil {
			return 0, e
		}
		waitUntil := sent[sequence].Add(interval)
		if sequence == PingRequests-1 && limited {
			waitUntil = deadline
		}
		_ = conn.SetReadDeadline(waitUntil)
		for {
			n, from, e := conn.ReadFrom(received)
			at := time.Now()
			if e != nil {
				if ctx.Err() != nil {
					return 0, ErrNoPingAnswer
				}
				if errors.Is(e, os.ErrDeadlineExceeded) {
					break
				}
				return 0, e
			}
			peer, isIP := from.(*net.IPAddr)
			if answered, valid := EchoReplySequence(received[:n], replyType, marker); valid && isIP && peer.IP.Equal(target.IP) && !sent[answered].IsZero() {
				return at.Sub(sent[answered]), nil
			}
		}
	}
	return 0, ErrNoPingAnswer
}

// EchoRequest writes one ICMP echo request: the type, a zero code, the
// checksum, two bytes that identify the sender, two bytes with the number of
// the request, and data. marker gives the identifying bytes and the data.
// The kernel fills in the checksum of a request of IP version 6 itself.
func EchoRequest(requestType byte, marker []byte, sequence int) []byte {
	message := append([]byte{requestType, 0, 0, 0, marker[0], marker[1], byte(sequence >> 8), byte(sequence)}, marker[2:]...)
	if requestType == 8 {
		checksum := InternetChecksum(message)
		message[2], message[3] = byte(checksum>>8), byte(checksum)
	}
	return message
}

// EchoReplySequence reads the number of the request from an ICMP echo
// reply that carries marker. Any other message is not valid.
func EchoReplySequence(message []byte, replyType byte, marker []byte) (sequence int, valid bool) {
	if len(message) != 8+len(marker)-2 || message[0] != replyType || message[1] != 0 || !bytes.Equal(message[4:6], marker[:2]) || !bytes.Equal(message[8:], marker[2:]) {
		return 0, false
	}
	sequence = int(message[6])<<8 | int(message[7])
	return sequence, sequence < PingRequests
}

// InternetChecksum is the checksum of the Internet protocols over a
// message: the complement of the sum of its 16-bit words, with each carry
// added back in.
func InternetChecksum(message []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(message); i += 2 {
		sum += uint32(message[i])<<8 | uint32(message[i+1])
	}
	if len(message)%2 == 1 {
		sum += uint32(message[len(message)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

// FileInformation is what the node says about the file at one path.
type FileInformation struct {
	// Kind says what is at the path: a regular file, a directory, and so
	// on. IsRegularFile and IsDirectory are set for the first two.
	Kind                       string
	IsRegularFile, IsDirectory bool
	SizeBytes                  int64
	// Modified is when the content was last changed.
	Modified        time.Time
	UserID, GroupID uint32
	// PermissionsMode is the mode as chmod takes it, such as 0644.
	PermissionsMode uint32
	// Inode and device together name the file itself, whatever its path:
	// a new file under the same path has another inode.
	Inode, Device uint64
}

// InformationAboutFile returns what the node says about the file at path.
// A symbolic link stands for the file that it names. A path without a file
// is an error that is fs.ErrNotExist.
func InformationAboutFile(path string) (FileInformation, error) {
	info, e := os.Stat(path)
	if e != nil {
		return FileInformation{}, e
	}
	return FileInformationFrom(info), nil
}

// ErrNotARegularFile reports a path that holds something else than a
// regular file.
var ErrNotARegularFile = errors.New("not a regular file")

// OpenRegularFile opens the regular file at path for reading, and returns
// it with what the node says about that open file. A symbolic link stands
// for the file that it names. A caller that reads a file opens it once and
// takes everything from the open file, so that a path at which one file
// takes the place of another shows one file only. A path that holds
// something else than a regular file is ErrNotARegularFile, with what it
// holds; a named pipe there does not hold the caller up.
func OpenRegularFile(path string) (*os.File, FileInformation, error) {
	return openRegularFile(path, 0)
}

// ErrSymbolicLink reports a path that is a symbolic link where the caller
// reads only the file at the path itself.
var ErrSymbolicLink = errors.New("a symbolic link")

// OpenRegularFileNotALink is OpenRegularFile for a file whose content goes
// into alerts, read as root: a symbolic link at path is ErrSymbolicLink.
// Reason: an account that can write the file's directory could otherwise
// put a link there to a file that only root may read.
func OpenRegularFileNotALink(path string) (*os.File, FileInformation, error) {
	f, about, e := openRegularFile(path, syscall.O_NOFOLLOW)
	if errors.Is(e, syscall.ELOOP) {
		return nil, FileInformation{Kind: "a symbolic link"}, ErrSymbolicLink
	}
	return f, about, e
}

func openRegularFile(path string, extraFlags int) (*os.File, FileInformation, error) {
	f, e := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|extraFlags, 0)
	if e != nil {
		return nil, FileInformation{}, e
	}
	info, e := f.Stat()
	if e != nil {
		_ = f.Close() // Closing this read-only handle has no pending writes to commit.
		return nil, FileInformation{}, e
	}
	about := FileInformationFrom(info)
	if !about.IsRegularFile {
		_ = f.Close() // Closing this read-only handle has no pending writes to commit.
		return nil, about, ErrNotARegularFile
	}
	return f, about, nil
}

// FileInformationFrom is what the node says about a file, from the answer
// of the kernel.
func FileInformationFrom(info os.FileInfo) FileInformation {
	mode := info.Mode()
	about := FileInformation{Kind: "a file of a special kind", IsRegularFile: mode.IsRegular(), IsDirectory: mode.IsDir(), SizeBytes: info.Size(), Modified: info.ModTime(), PermissionsMode: uint32(mode.Perm())}
	switch {
	case mode.IsRegular():
		about.Kind = "a regular file"
	case mode.IsDir():
		about.Kind = "a directory"
	case mode&os.ModeSocket != 0:
		about.Kind = "a socket"
	case mode&os.ModeNamedPipe != 0:
		about.Kind = "a named pipe"
	case mode&os.ModeDevice != 0:
		about.Kind = "a device"
	}
	for bit, value := range map[os.FileMode]uint32{os.ModeSetuid: 04000, os.ModeSetgid: 02000, os.ModeSticky: 01000} {
		if mode&bit != 0 {
			about.PermissionsMode |= value
		}
	}
	if owner, known := info.Sys().(*syscall.Stat_t); known {
		about.UserID, about.GroupID, about.Inode, about.Device = owner.Uid, owner.Gid, uint64(owner.Ino), uint64(owner.Dev)
	}
	return about
}

// Sha256ChecksumOfFile reads the whole of an open file and returns the
// SHA-256 checksum of its content, as 64 hex characters. ctx ends the
// reading of a file that is too large for the time that the caller has.
func Sha256ChecksumOfFile(ctx context.Context, f *os.File) (string, error) {
	sum, part, offset := sha256.New(), make([]byte, 1024*1024), int64(0)
	for {
		if e := ctx.Err(); e != nil {
			return "", e
		}
		n, e := f.ReadAt(part, offset)
		sum.Write(part[:n])
		offset += int64(n)
		if e == io.EOF {
			return hex.EncodeToString(sum.Sum(nil)), nil
		}
		if e != nil {
			return "", e
		}
	}
}

// UserAccountName returns the name of the user account with an ID, or the
// ID as a number when the node has no account with it. An account that is
// not in the node's own files, such as one of a directory service, is
// found through the node's getent program.
func UserAccountName(ctx context.Context, id uint32) string {
	number := strconv.FormatUint(uint64(id), 10)
	if account, e := user.LookupId(number); e == nil {
		return account.Username
	}
	return nameFromGetent(ctx, "passwd", number)
}

// GroupName returns the name of the group with an ID, or the ID as a number
// when the node has no group with it. A group that is not in the node's own
// files is found through the node's getent program.
func GroupName(ctx context.Context, id uint32) string {
	number := strconv.FormatUint(uint64(id), 10)
	if group, e := user.LookupGroupId(number); e == nil {
		return group.Name
	}
	return nameFromGetent(ctx, "group", number)
}

// nameFromGetent asks the node's getent program for the name that belongs
// to a number in one of its databases, passwd or group. getent asks every
// source of accounts that the node is set up with. The answer is one line
// that starts with the name and a colon. Without such an answer the name
// is the number itself.
func nameFromGetent(ctx context.Context, database, number string) string {
	answer, e := RunCommand(ctx, Command{Arguments: []string{"getent", database, number}, RunAs: &ExecOptions{RunAsUserAccount: UnprivilegedAccount, RunFromDirectory: "/"}})
	if name, _, found := strings.Cut(answer.StandardOutput, ":"); e == nil && answer.ExitCode == 0 && found && name != "" && !strings.ContainsAny(name, "\n ") {
		return name
	}
	return number
}

// ReadFileFrom reads an open file from an offset, at most limit bytes.
func ReadFileFrom(f *os.File, offset int64, limit int) ([]byte, error) {
	return io.ReadAll(io.NewSectionReader(f, offset, int64(limit)))
}

// SQLiteQuery opens the SQLite database file at path for reading only, runs
// one query, and returns the first column of each row as text. ctx ends a
// query that takes too long. The connection can change nothing in the
// database. It is an ordinary reader of the database, with SQLite's locks,
// and it sees one consistent state for as long as its query takes. A
// database that another program holds locked is waited for until ctx ends.
func SQLiteQuery(ctx context.Context, path, query string) ([]string, error) {
	wait := 5 * time.Second
	if deadline, limited := ctx.Deadline(); limited {
		wait = max(time.Until(deadline), 0)
	}
	// The path is written as a file: address, so that a path with a
	// question mark or a percent sign is still that path.
	address := (&url.URL{Scheme: "file", Path: path}).String() + fmt.Sprintf("?mode=ro&_pragma=busy_timeout(%d)&_pragma=query_only(1)", wait.Milliseconds())
	db, e := sql.Open("sqlite", address)
	if e != nil {
		return nil, e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	rows, e := db.QueryContext(ctx, query)
	if e != nil {
		return nil, e
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	values := []string{}
	for rows.Next() {
		var value sql.NullString
		if e := rows.Scan(&value); e != nil {
			return nil, e
		}
		values = append(values, value.String)
	}
	return values, rows.Err()
}

// SystemLogSocket is where systemd's journal takes the entries of
// programs. A test puts a socket of its own in its place.
var SystemLogSocket = "/run/systemd/journal/socket"

// ConnectSystemLog opens the journal's socket without writing an entry.
// Verification and alert delivery use the same connection operation.
func ConnectSystemLog(ctx context.Context) (net.Conn, error) {
	return (&net.Dialer{}).DialContext(ctx, "unixgram", SystemLogSocket)
}

// WriteToSystemLog writes one entry to the system log of the machine, the
// systemd journal: a message, its priority as syslog counts it, from 0 for
// an emergency to 7 for debugging, and the name of the program that the
// entry is from, which "journalctl --identifier" selects. An entry that the
// journal does not take is an error.
func WriteToSystemLog(ctx context.Context, priority int, identifier, message string) error {
	conn, e := ConnectSystemLog(ctx)
	if e != nil {
		return e
	}
	defer conn.Close()
	// The system log can be slow to take a line, or take none at all. The
	// write ends with ctx, as the connect does.
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	// The journal reads one field on each line, as NAME=value. A value of
	// several lines is written as its name, its length in 8 bytes, and the
	// value.
	entry := []byte(fmt.Sprintf("PRIORITY=%d\nSYSLOG_IDENTIFIER=%s\n", priority, identifier))
	if strings.Contains(message, "\n") {
		entry = append(entry, "MESSAGE\n"...)
		entry = binary.LittleEndian.AppendUint64(entry, uint64(len(message)))
		entry = append(append(entry, message...), '\n')
	} else {
		entry = append(entry, "MESSAGE="+message+"\n"...)
	}
	_, e = conn.Write(entry)
	return e
}

// errReadLimit reports a server that sent more than MMWatchdog reads from it.
var errReadLimit = errors.New("the server sent more data than MMWatchdog reads from the server")

// ReadLimitedConnection is a connection from which MMWatchdog reads at most a
// fixed number of bytes. A reader that takes a whole answer into memory, as
// Go's SMTP client does, then cannot be made to take more than that: the
// next read after the limit is errReadLimit.
type ReadLimitedConnection struct {
	net.Conn
	Remaining int
}

func (c *ReadLimitedConnection) Read(p []byte) (int, error) {
	if c.Remaining <= 0 {
		return 0, errReadLimit
	}
	n, err := c.Conn.Read(p[:min(len(p), c.Remaining)])
	c.Remaining -= n
	return n, err
}

// ConnectionDidNotOpen is the error of a connection that did not open, in
// the words of that error. A caller tells it apart from a connection that
// opened and then gave no answer.
type ConnectionDidNotOpen struct{ error }

func (e ConnectionDidNotOpen) Unwrap() error { return e.error }

// TCPExchangeLimit is the most that TCPExchange reads from a connection.
const TCPExchangeLimit = 64 * 1024

// TCPExchange opens a TCP connection to address, written as host:port. With
// textToSend it then sends that text. With readUntilText it reads until what
// it has received holds that text, the other side closes the connection, or
// it has read TCPExchangeLimit bytes, and returns what it received. With
// neither, a connection that opens is all that it establishes. ctx ends an
// exchange that takes too long.
func TCPExchange(ctx context.Context, address, textToSend, readUntilText string) (string, error) {
	// Every text holds the empty text, so without a text to wait for the
	// exchange reads nothing.
	return TCPExchangeUntil(ctx, address, textToSend, func(received []byte) bool { return bytes.Contains(received, []byte(readUntilText)) })
}

// TCPExchangeUntil is TCPExchange for a caller that decides by itself when
// it has received enough: it reads until complete answers yes for what it
// has received so far.
func TCPExchangeUntil(ctx context.Context, address, textToSend string, complete func(received []byte) bool) (string, error) {
	return TCPExchangeUntilWithTLS(ctx, address, nil, textToSend, complete)
}

// TCPExchangeUntilWithTLS encrypts the exchange before sending any text when secure is non-nil.
func TCPExchangeUntilWithTLS(ctx context.Context, address string, secure *tls.Config, textToSend string, complete func(received []byte) bool) (string, error) {
	conn, err := ConnectTCP(ctx, address)
	if err != nil {
		return "", ConnectionDidNotOpen{err}
	}
	defer conn.Close()
	// The connection has no time limit of its own: closing it when ctx ends
	// stops a read or a write that waits.
	rawConnection := conn
	stop := context.AfterFunc(ctx, func() { rawConnection.Close() })
	defer stop()
	ended := func(e error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return e
	}
	if secure != nil {
		encrypted := tls.Client(conn, secure)
		if err := encrypted.HandshakeContext(ctx); err != nil {
			return "", ConnectionDidNotOpen{ended(err)}
		}
		conn = encrypted
	}
	if textToSend != "" {
		if _, e := io.WriteString(conn, textToSend); e != nil {
			return "", ended(e)
		}
	}
	received := []byte{}
	part := make([]byte, 4096)
	for !complete(received) && len(received) < TCPExchangeLimit {
		n, e := conn.Read(part[:min(len(part), TCPExchangeLimit-len(received))])
		received = append(received, part[:n]...)
		if e == io.EOF {
			break
		}
		if e != nil {
			return string(received), ended(e)
		}
	}
	return string(received), nil
}

// eachAddressOf calls try with each address of the host in address, written
// as host:port, until one call succeeds, and returns that call's answer. A
// host name can have several addresses, such as localhost with one of each
// family, and a program often listens on one of them only. TCP tries each
// address by itself; a caller that sends UDP packets uses this. Each try
// gets an equal share of the time that ctx leaves. When no address answers,
// the error is the one of the last try, or context.DeadlineExceeded when
// that try ran out of time.
func eachAddressOf[T any](ctx context.Context, address string, try func(ctx context.Context, address string) (T, error)) (T, error) {
	var none T
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return none, err
	}
	addresses, err := ResolveHostName(ctx, host)
	if err != nil {
		return none, err
	}
	if len(addresses) == 0 {
		return none, fmt.Errorf("the host name %s has no IP address", host)
	}
	for i, one := range addresses {
		// The last address takes ctx itself: the time that remains is all
		// of ctx's, and a deadline of its own, computed from the clock a
		// moment later, would end a little before ctx does.
		attempt, cancel := ctx, context.CancelFunc(func() {})
		if deadline, limited := ctx.Deadline(); limited && i < len(addresses)-1 {
			attempt, cancel = context.WithDeadline(ctx, time.Now().Add(time.Until(deadline)/time.Duration(len(addresses)-i)))
		}
		var answer T
		answer, err = try(attempt, net.JoinHostPort(one.String(), port))
		ranOutOfTime := attempt.Err() != nil
		cancel()
		if err == nil {
			return answer, nil
		}
		if ranOutOfTime {
			err = context.DeadlineExceeded
		}
		if ctx.Err() != nil {
			break
		}
	}
	return none, err
}

// UDPExchange sends text in one UDP packet to address, written as
// host:port, and returns the content of the first packet that comes back,
// at most TCPExchangeLimit bytes. ctx ends the wait for an answer. UDP has
// no connection, so only an answer says that a program listens; a machine
// that reports that no program does is an error at once. Each address of a
// host name is tried (eachAddressOf).
func UDPExchange(ctx context.Context, address, textToSend string) (string, error) {
	return eachAddressOf(ctx, address, func(ctx context.Context, address string) (string, error) {
		received, err := udpPacketExchange(ctx, address, []byte(textToSend), func([]byte) bool { return true })
		return string(received), err
	})
}

// udpPacketExchange sends one UDP packet to address and returns the first
// packet that comes back and that isTheAnswer accepts.
func udpPacketExchange(ctx context.Context, address string, packet []byte, isTheAnswer func([]byte) bool) ([]byte, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "udp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	ended := func(e error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return e
	}
	if _, e := conn.Write(packet); e != nil {
		return nil, ended(e)
	}
	for {
		received := make([]byte, TCPExchangeLimit)
		n, e := conn.Read(received)
		if e != nil {
			return nil, ended(e)
		}
		if isTheAnswer(received[:n]) {
			return received[:n], nil
		}
	}
}

// The kinds of DNS record that DNSLookup asks for.
var DNSRecordTypes = []string{"A", "AAAA", "CNAME", "MX", "NS", "TXT", "PTR", "SRV"}

// dnsRecordTypeNumbers are the kinds of record as a DNS message names them.
var dnsRecordTypeNumbers = map[string]dnsmessage.Type{"A": dnsmessage.TypeA, "AAAA": dnsmessage.TypeAAAA, "CNAME": dnsmessage.TypeCNAME, "MX": dnsmessage.TypeMX,
	"NS": dnsmessage.TypeNS, "TXT": dnsmessage.TypeTXT, "PTR": dnsmessage.TypePTR, "SRV": dnsmessage.TypeSRV}

// ErrNoDNSRecord reports that the DNS server answered, and has no record
// of the kind for the name.
var ErrNoDNSRecord = errors.New("the DNS server has no record matching the requested name and record type")

// DNSQuestion writes the question for the records of one kind for a name,
// as a DNS message with the given ID. For PTR the name is an address, and
// the question asks for the name that DNS keeps for it. The name is taken
// as complete: MMWatchdog adds no search domain of the node to it. A name that DNS
// cannot hold is an error: a part of more than 63 characters, a name of
// more than 253, or a character that is not ASCII.
func DNSQuestion(id uint16, name, recordType string) ([]byte, dnsmessage.Name, error) {
	kind, known := dnsRecordTypeNumbers[recordType]
	if !known {
		return nil, dnsmessage.Name{}, fmt.Errorf("unsupported DNS record type %s", recordType)
	}
	asked := strings.TrimSuffix(name, ".") + "."
	if recordType == "PTR" {
		address, err := netip.ParseAddr(name)
		if err != nil {
			return nil, dnsmessage.Name{}, fmt.Errorf("a PTR lookup requires an IP address, but %s is not an IP address", name)
		}
		asked = reverseDNSName(address)
	}
	for _, part := range strings.Split(strings.TrimSuffix(asked, "."), ".") {
		if part == "" || len(part) > 63 {
			return nil, dnsmessage.Name{}, errors.New("each part of a DNS name between dots must have 1 to 63 characters")
		}
	}
	for i := 0; i < len(asked); i++ {
		if asked[i] <= ' ' || asked[i] >= 0x7f {
			return nil, dnsmessage.Name{}, errors.New("a DNS name must contain only printable ASCII characters without spaces; use the xn-- form for a name with non-ASCII characters")
		}
	}
	questionName, err := dnsmessage.NewName(asked)
	if err != nil || len(asked) > 254 {
		return nil, dnsmessage.Name{}, errors.New("a DNS name has at most 253 characters")
	}
	message := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: id, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: questionName, Type: kind, Class: dnsmessage.ClassINET}},
	}
	packed, err := message.Pack()
	return packed, questionName, err
}

// reverseDNSName returns the name under which DNS keeps the PTR record of
// an address, such as 10.2.0.192.in-addr.arpa. for 192.0.2.10.
func reverseDNSName(address netip.Addr) string {
	address = address.Unmap()
	if address.Is4() {
		b := address.As4()
		return fmt.Sprintf("%d.%d.%d.%d.in-addr.arpa.", b[3], b[2], b[1], b[0])
	}
	const digits = "0123456789abcdef"
	b, name := address.As16(), []byte{}
	for i := len(b) - 1; i >= 0; i-- {
		name = append(name, digits[b[i]&0xf], '.', digits[b[i]>>4], '.')
	}
	return string(name) + "ip6.arpa."
}

// DNSLookup asks the DNS server at server, written as host:port, for the
// records of one kind for a name, and returns their values: an address for
// A and AAAA, a host name for CNAME, MX, NS, and PTR, the text for TXT, and
// host:port for SRV. Host names come without the dot at their end, except
// the root name, which is ".": the host of a null MX record (RFC 7505),
// which says that the domain takes no mail, and the target of an SRV record
// that says that the service is not available (RFC 2782). Such an SRV
// record is "." alone, as its port means nothing.
//
// MMWatchdog writes the question itself and sends it to that server and to no
// other: the node's own resolver, its hosts file, and its settings take no
// part, so the answer is the one of the server that the caller names. The
// question goes in one UDP packet, and again over TCP when the server says
// that its answer does not fit a packet. A server that has no such record
// is ErrNoDNSRecord.
func DNSLookup(ctx context.Context, server, name, recordType string) ([]string, error) {
	var random [2]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	id := binary.BigEndian.Uint16(random[:])
	question, asked, err := DNSQuestion(id, name, recordType)
	if err != nil {
		return nil, err
	}
	answer, err := eachAddressOf(ctx, server, func(ctx context.Context, address string) ([]byte, error) {
		received, err := udpPacketExchange(ctx, address, question, func(packet []byte) bool {
			// A packet with another ID answers another question.
			return len(packet) >= 12 && binary.BigEndian.Uint16(packet) == id
		})
		if err != nil {
			return nil, err
		}
		// The third byte of an answer says that it was cut to fit a packet.
		if received[2]&0x02 != 0 {
			return dnsExchangeOverTCP(ctx, address, question)
		}
		return received, nil
	})
	if err != nil {
		return nil, err
	}
	var parser dnsmessage.Parser
	header, err := parser.Start(answer)
	if err != nil || header.ID != id || !header.Response {
		return nil, errors.New("the DNS server sent an invalid response or a response to a different query")
	}
	switch header.RCode {
	case dnsmessage.RCodeSuccess:
	case dnsmessage.RCodeNameError:
		return nil, ErrNoDNSRecord
	case dnsmessage.RCodeRefused:
		return nil, errors.New("the DNS server refused the query")
	case dnsmessage.RCodeServerFailure:
		return nil, errors.New("the DNS server reported a failure while processing the query")
	default:
		return nil, fmt.Errorf("the DNS server returned error %s", strings.TrimPrefix(header.RCode.String(), "RCode"))
	}
	if err := parser.SkipAllQuestions(); err != nil {
		return nil, errors.New("the DNS server sent a response that MMWatchdog cannot read")
	}
	hostName := func(host dnsmessage.Name) string {
		if written := host.String(); written != "." {
			return strings.TrimSuffix(written, ".")
		}
		return "."
	}
	kind, values := dnsRecordTypeNumbers[recordType], []string{}
	for {
		record, err := parser.AnswerHeader()
		if errors.Is(err, dnsmessage.ErrSectionDone) {
			break
		}
		if err != nil {
			return nil, errors.New("the DNS server sent a response that MMWatchdog cannot read")
		}
		// An answer can hold records of another kind on the way to the one
		// that was asked for, such as the CNAME record of a name before
		// the address of the name that it stands for.
		if record.Type != kind || (kind == dnsmessage.TypeCNAME && !strings.EqualFold(record.Name.String(), asked.String())) {
			if err := parser.SkipAnswer(); err != nil {
				return nil, errors.New("the DNS server sent a response that MMWatchdog cannot read")
			}
			continue
		}
		var value string
		// kind comes from dnsRecordTypeNumbers, which contains only the supported DNS record types handled here.
		//exhaustive:ignore
		switch kind {
		case dnsmessage.TypeA:
			var r dnsmessage.AResource
			r, err = parser.AResource()
			value = netip.AddrFrom4(r.A).String()
		case dnsmessage.TypeAAAA:
			var r dnsmessage.AAAAResource
			r, err = parser.AAAAResource()
			value = netip.AddrFrom16(r.AAAA).Unmap().String()
		case dnsmessage.TypeCNAME:
			var r dnsmessage.CNAMEResource
			r, err = parser.CNAMEResource()
			value = hostName(r.CNAME)
		case dnsmessage.TypeMX:
			var r dnsmessage.MXResource
			r, err = parser.MXResource()
			value = hostName(r.MX)
		case dnsmessage.TypeNS:
			var r dnsmessage.NSResource
			r, err = parser.NSResource()
			value = hostName(r.NS)
		case dnsmessage.TypePTR:
			var r dnsmessage.PTRResource
			r, err = parser.PTRResource()
			value = hostName(r.PTR)
		case dnsmessage.TypeTXT:
			var r dnsmessage.TXTResource
			r, err = parser.TXTResource()
			// One record can hold its text in several pieces.
			value = strings.Join(r.TXT, "")
		case dnsmessage.TypeSRV:
			var r dnsmessage.SRVResource
			r, err = parser.SRVResource()
			value = hostName(r.Target)
			if value != "." {
				value = net.JoinHostPort(value, strconv.Itoa(int(r.Port)))
			}
		}
		if err != nil {
			return nil, errors.New("the DNS server sent a response that MMWatchdog cannot read")
		}
		values = append(values, value)
	}
	if len(values) == 0 {
		return nil, ErrNoDNSRecord
	}
	return values, nil
}

// dnsExchangeOverTCP sends a DNS question over TCP to address and returns
// the answer. Over TCP, each message has its length in the two bytes in
// front of it.
func dnsExchangeOverTCP(ctx context.Context, address string, question []byte) ([]byte, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	ended := func(e error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return e
	}
	if _, e := conn.Write(append(binary.BigEndian.AppendUint16(nil, uint16(len(question))), question...)); e != nil {
		return nil, ended(e)
	}
	var length [2]byte
	if _, e := io.ReadFull(conn, length[:]); e != nil {
		return nil, ended(e)
	}
	answer := make([]byte, binary.BigEndian.Uint16(length[:]))
	if _, e := io.ReadFull(conn, answer); e != nil {
		return nil, ended(e)
	}
	return answer, nil
}

// TLSCertificateExpiry opens a TLS connection to address, written as
// host:port, verifies the server's certificate for serverName, and returns
// when that certificate expires. certificateAuthorities replace the system
// trust store when they are not nil.
func TLSCertificateExpiry(ctx context.Context, address, serverName string, certificateAuthorities *x509.CertPool) (time.Time, error) {
	secure := ServerTLSConfig(certificateAuthorities, false)
	secure.MinVersion, secure.ServerName = tls.VersionTLS12, serverName
	dialer := tls.Dialer{Config: secure}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return time.Time{}, err
	}
	defer conn.Close()
	return conn.(*tls.Conn).ConnectionState().PeerCertificates[0].NotAfter, nil
}

const ResultLimit = 64 * 1024

const DiagnosticLimit = 16 * 1024

// DetailLinesLimit is the most lines that the details of one observation
// hold, and DetailLineLimit the most bytes of one of them.
const (
	DetailLinesLimit = 12
	DetailLineLimit  = 200
)

// DetailLine makes one line of an observation's details from text that the
// node gave, such as a command line or a line of a log: on one line, with
// each control character replaced, and no longer than DetailLineLimit.
func DetailLine(text string) string {
	return Clip(OnOneLine(text), DetailLineLimit)
}

// OnOneLine puts text that the node gave on one line, with each control
// character replaced, and with each character replaced that changes how
// the text around it is shown.
func OnOneLine(text string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == utf8.RuneError || changesHowTextIsShown(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(text, " ")))
}

// AsReadableLines keeps text that the node gave on its lines, with its line
// breaks and tabs, and replaces every other control character, and each
// character that changes how the text around it is shown, with a space. A
// receiver of an alert can refuse text with such a character, such as a
// NUL, which a database of text cannot hold, and then every later alert to
// that destination would wait behind the one it refuses.
func AsReadableLines(text string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == utf8.RuneError || changesHowTextIsShown(r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(text, " "))
}

// changesHowTextIsShown reports a character that is no control character by
// Unicode's rule and still changes how the text around it is shown: the two
// line separators, and the characters that turn the direction of text, with
// which a line can show its words in another order than it holds them.
func changesHowTextIsShown(r rune) bool {
	return r == 0x2028 || r == 0x2029 || r == 0x061c || r == 0x200e || r == 0x200f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069)
}

type limitedBuffer struct {
	mu       sync.Mutex
	b        bytes.Buffer
	limit    int
	overflow bool
	// whenOver, when set, is called once more than the limit was written.
	whenOver func()
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	room := b.limit - b.b.Len()
	if n > room {
		b.overflow = true
		p = p[:room]
		if b.whenOver != nil {
			b.whenOver()
		}
	}
	b.b.Write(p)
	return n, nil
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.ToValidUTF8(b.b.String(), "�")
}

func Clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}

// ProcFileLimit is the most that MMWatchdog reads of one file under /proc.
const ProcFileLimit = 1024 * 1024

// FilesystemCounts are the statfs numbers of one filesystem. Blocks are
// counted in units of blockBytes, and blocksAvailable are those an ordinary
// user can still fill.
type FilesystemCounts struct {
	Blocks, BlocksAvailable, BlockBytes uint64
	Inodes, InodesFree                  uint64
	ReadOnly                            bool
}

// SpaceUsedPercent is the share of the filesystem's space that is in use,
// or false for counts that say nothing.
func (c FilesystemCounts) SpaceUsedPercent() (float64, bool) {
	if c.Blocks == 0 || c.BlocksAvailable > c.Blocks {
		return 0, false
	}
	return 100 * (1 - float64(c.BlocksAvailable)/float64(c.Blocks)), true
}

// ExecOptions say how a shell check, an external module, or a recovery
// command runs.
type ExecOptions struct {
	RunAsUserAccount string            `yaml:"run_as_user_account" json:"run_as_user_account"`
	RunFromDirectory string            `yaml:"run_from_directory" json:"run_from_directory"`
	Env              map[string]string `yaml:"env" json:"env,omitempty"`
}

func ParseID(s string) (uint32, error) { n, e := strconv.ParseUint(s, 10, 32); return uint32(n), e }

// MySQLTLSLoginGuard gives MariaDB's client a loopback address that forwards
// to address only after the client requests TLS. Older MariaDB clients can
// otherwise send a login unencrypted when certificate checking is disabled.
// stop closes the listener and every connection belonging to this run.
func MySQLTLSLoginGuard(ctx context.Context, address string) (string, func(), error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stopListener := context.AfterFunc(ctx, func() { listener.Close() })
	go func() {
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				stopClient := context.AfterFunc(ctx, func() { client.Close() })
				defer stopClient()
				server, err := ConnectTCP(ctx, address)
				if err != nil {
					return
				}
				defer server.Close()
				stopServer := context.AfterFunc(ctx, func() { server.Close() })
				defer stopServer()
				// Forward one bounded greeting. No client login has been sent.
				var header [4]byte
				if _, err := io.ReadFull(server, header[:]); err != nil {
					return
				}
				length := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
				if length > TCPExchangeLimit {
					return
				}
				greeting := make([]byte, length)
				if _, err := io.ReadFull(server, greeting); err != nil {
					return
				}
				if _, err := client.Write(append(header[:], greeting...)); err != nil {
					return
				}
				// The SSLRequest is exactly 32 bytes, with CLIENT_SSL (0x800)
				// set. A handshake response carrying a login is never forwarded.
				if _, err := io.ReadFull(client, header[:]); err != nil {
					return
				}
				if header[0] != 32 || header[1] != 0 || header[2] != 0 {
					return
				}
				request := make([]byte, 32)
				if _, err := io.ReadFull(client, request); err != nil {
					return
				}
				if binary.LittleEndian.Uint32(request[:4])&0x800 == 0 {
					return
				}
				if _, err := server.Write(append(header[:], request...)); err != nil {
					return
				}
				// From here the client performs TLS before sending the login.
				done := make(chan struct{})
				go func() { _, _ = io.Copy(server, client); server.Close(); close(done) }()
				_, _ = io.Copy(client, server)
				client.Close()
				<-done
			}()
		}
	}()
	return listener.Addr().String(), func() { cancel(); stopListener(); listener.Close() }, nil
}
