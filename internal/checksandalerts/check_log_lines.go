package checksandalerts

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// LogLines is the inputs of the log_lines check module, which reads the
// lines that a program adds to a log: to a file, or to the journal of a
// systemd unit. A check states exactly one source, LogFilePath or
// JournalOfSystemdUnit, and exactly one condition.
type LogLines struct {
	LogFilePath                  string   `yaml:"log_file_path" json:"log_file_path,omitempty"`
	JournalOfSystemdUnit         string   `yaml:"journal_of_systemd_unit" json:"journal_of_systemd_unit,omitempty"`
	FailWhenANewLineContains     string   `yaml:"fail_when_a_new_line_contains" json:"fail_when_a_new_line_contains,omitempty"`
	FailWhenANewLineMatchesRegex string   `yaml:"fail_when_a_new_line_matches_regex" json:"fail_when_a_new_line_matches_regex,omitempty"`
	FailWhenNoNewLineFor         Duration `yaml:"fail_when_no_new_line_for" json:"fail_when_no_new_line_for,omitempty"`
}

func (l *LogLines) Validate(v InputValidation) *FieldError {
	stated := func(names ...string) []string {
		found := []string{}
		for _, name := range names {
			if v.Authored(name) {
				found = append(found, name)
			}
		}
		return found
	}
	sources := stated("log_file_path", "journal_of_systemd_unit")
	if len(sources) != 1 {
		return &FieldError{"", "states exactly one source: log_file_path or journal_of_systemd_unit"}
	}
	conditions := stated("fail_when_a_new_line_contains", "fail_when_a_new_line_matches_regex", "fail_when_no_new_line_for")
	if len(conditions) != 1 {
		return &FieldError{"", "states exactly one condition: fail_when_a_new_line_contains, fail_when_a_new_line_matches_regex, or fail_when_no_new_line_for"}
	}
	switch sources[0] {
	case "log_file_path":
		if !filepath.IsAbs(l.LogFilePath) || filepath.Clean(l.LogFilePath) != l.LogFilePath {
			return &FieldError{"log_file_path", "must be the absolute path of the log file"}
		}
	default:
		if !primitives.AnyUnitPattern.MatchString(l.JournalOfSystemdUnit) {
			return &FieldError{"journal_of_systemd_unit", "must be the name of a systemd unit with its kind, such as myapp.service"}
		}
	}
	switch conditions[0] {
	case "fail_when_a_new_line_contains":
		if l.FailWhenANewLineContains == "" || strings.ContainsAny(l.FailWhenANewLineContains, "\r\n") {
			return &FieldError{"fail_when_a_new_line_contains", "must be a text on one line that is not empty"}
		}
	case "fail_when_a_new_line_matches_regex":
		if l.FailWhenANewLineMatchesRegex == "" {
			return &FieldError{"fail_when_a_new_line_matches_regex", "cannot be empty"}
		}
		if _, err := regexp.Compile(l.FailWhenANewLineMatchesRegex); err != nil {
			return &FieldError{"fail_when_a_new_line_matches_regex", "is no valid regular expression: " + err.Error()}
		}
	default:
		if l.FailWhenNoNewLineFor.Value() <= 0 {
			return &FieldError{"fail_when_no_new_line_for", "expected a duration longer than 0s, such as 10m"}
		}
	}
	return nil
}

// The most bytes that one run reads, and the most bytes matched in one line.
// A run with more new lines continues at the next run.
const (
	logLinesReadLimit = 16 * 1024 * 1024
	logLinesPartLimit = 1024 * 1024
	logLineLimit      = 64 * 1024
	matchingLinesKept = 5
)

// logFileStartLimit is how many of a log file's first bytes MMWatchdog remembers a
// checksum of, to know the file again.
const logFileStartLimit = 512

// LogLinesRead is what a log_lines check keeps between its runs: how far it
// has read, and when the source last got a new line.
type LogLinesRead struct {
	// WatchingSince is the time of the check's first run.
	WatchingSince time.Time `json:"watching_since"`
	// Inode names the log file itself, and Offset is how many bytes of it
	// the check has read. StartChecksum is the SHA-256 checksum of the
	// file's first StartLength bytes. A program that rotates its log starts
	// a new file under the same name: another inode, a shorter file, or
	// another start. The number of the file's device is no part of this:
	// a device can get another number when the machine starts.
	Inode  uint64 `json:"inode,omitempty"`
	Offset int64  `json:"offset,omitempty"`
	// An oversized line can span several read budgets. Its first 64 KiB
	// is checked once; subsequent runs discard its remainder, not new lines.
	SkippingOversizedLine bool   `json:"skipping_oversized_line,omitempty"`
	StartLength           int    `json:"start_length,omitempty"`
	StartChecksum         string `json:"start_checksum,omitempty"`
	// Cursor is the place of the last entry of the journal that the check
	// has read, as journalctl names it.
	Cursor string `json:"cursor,omitempty"`
	// NewestLineAt is when the source last got a line.
	NewestLineAt time.Time `json:"newest_line_at,omitzero"`
}

// logFileHeld is what a log_lines check of a file remembers while MMWatchdog runs:
// the log file as the run before left it, still open. When the program
// starts a new file under the name, the next run reads the old file to its
// end through this, and so misses none of its last lines.
type logFileHeld struct {
	file  *os.File
	inode uint64
}

// newLogLines is what one run read: how many new lines, which of them the
// condition matched, when the newest of them was written, and whether the
// source holds more new lines than the run read.
type newLogLines struct {
	count        int
	matching     int
	firstMatches []string
	newestLineAt time.Time
	more         bool
}

func (l *LogLines) Run(ctx context.Context, r *CheckRun) Observation {
	var read LogLinesRead
	r.State.load(&read)
	first := read.WatchingSince.IsZero()
	if first {
		read.WatchingSince = time.Now().UTC()
	}
	// Each new line is looked at as it is read, and then let go: a run
	// keeps the numbers and the first matching lines, and never the log.
	fresh := newLogLines{}
	matches, whatOneLineDoes, whatSeveralLinesDo := l.matcher()
	line := func(text string) bool {
		fresh.count++
		if matches == nil || !matches(text) {
			return false
		}
		if fresh.matching++; len(fresh.firstMatches) < matchingLinesKept {
			fresh.firstMatches = append(fresh.firstMatches, primitives.DetailLine(text))
		}
		return true
	}
	source, problem := "the journal of "+l.JournalOfSystemdUnit, ""
	if l.LogFilePath != "" {
		source = l.LogFilePath
		problem = l.readFile(ctx, r, &read, first, &fresh, line)
	} else {
		problem = l.readJournal(ctx, &read, first, &fresh, line)
	}

	if fresh.newestLineAt.After(read.NewestLineAt) {
		read.NewestLineAt = fresh.newestLineAt
	}
	if first && problem != "" {
		return Result("unknown", problem)
	}
	r.State.Save(read)
	if problem != "" && fresh.matching == 0 {
		return Result("unknown", problem)
	}
	// The first run starts at the end of the log, so the lines that were
	// there before cause no alert.
	if first {
		return Result("ok", "MMWatchdog starts watching new lines in "+source+" from now on")
	}

	if silence := l.FailWhenNoNewLineFor.Value(); silence > 0 {
		newest, since := read.NewestLineAt, "since the newest line"
		if newest.IsZero() {
			newest, since = read.WatchingSince, "since MMWatchdog started watching the log"
		}
		quiet := time.Since(newest).Truncate(time.Second)
		if quiet > silence {
			return Result("failed", fmt.Sprintf("%s got no new line for %s %s (fails after %s)", source, quiet, since, l.FailWhenNoNewLineFor))
		}
		return Result("ok", fmt.Sprintf("%s received %s; no new line in the last %s (fails after %s)", source, countOf(fresh.count, "new line", "new lines"), quiet, l.FailWhenNoNewLineFor))
	}

	if fresh.matching > 0 {
		what := whatSeveralLinesDo
		if fresh.matching == 1 {
			what = whatOneLineDoes
		}
		o := Result("failed", fmt.Sprintf("%s in %s %s", countOf(fresh.matching, "new line", "new lines"), source, what))
		if problem != "" {
			o.Message += "; " + problem
		}
		o.Details = fresh.firstMatches
		if more := fresh.matching - len(fresh.firstMatches); more > 0 {
			o.Details = append(o.Details, fmt.Sprintf("and %d more", more))
		}
		return o
	}
	if fresh.more {
		return Result("unknown", fmt.Sprintf("%s has more new lines than MMWatchdog reads in one run, or than MMWatchdog could read within the time limit; the next run continues where the current run stopped", source))
	}
	return Result("ok", fmt.Sprintf("none of %s in %s %s", countOf(fresh.count, "new line", "new lines"), source, whatOneLineDoes))
}

// matcher returns the condition on one line, and its words for a message:
// what one line does that meets it, and what several lines do.
// A check of silence has no condition on a line. MMWatchdog looks at no more than
// logLineLimit of a line.
func (l *LogLines) matcher() (matches func(line string) bool, whatOneLineDoes, whatSeveralLinesDo string) {
	switch {
	case l.FailWhenANewLineContains != "":
		text := strconv.Quote(l.FailWhenANewLineContains)
		return func(line string) bool {
			return strings.Contains(line, l.FailWhenANewLineContains)
		}, "contains " + text, "contain " + text
	case l.FailWhenANewLineMatchesRegex != "":
		pattern := regexp.MustCompile(l.FailWhenANewLineMatchesRegex)
		return func(line string) bool { return pattern.MatchString(line) }, "matches the regular expression " + l.FailWhenANewLineMatchesRegex, "match the regular expression " + l.FailWhenANewLineMatchesRegex
	}
	return nil, "", ""
}

// ReadFile reads the lines that the log file got since the run before,
// gives each to line, and moves read to where it stopped. It returns what
// kept it from reading, or "".
//
// The file under the name can be another one than at the run before: the
// program, or logrotate, renamed the old file and started a new one. While
// MMWatchdog runs, the check keeps the file of the run before open, reads that file
// to its end first, and only then starts the new one, so the last lines of
// the old file are read. After a start of MMWatchdog the check has only the name,
// and knows the file again by its inode and by a checksum of its first
// bytes; whatever else it finds under the name, it reads from the start. A
// file that was emptied in place is read from its start too, and the lines
// that it held between the run before and that moment are gone with it.
func (l *LogLines) readFile(ctx context.Context, r *CheckRun, read *LogLinesRead, first bool, fresh *newLogLines, line func(string) bool) string {
	held, _ := r.Memory.Value.(*logFileHeld)
	forget := func() {
		if held != nil {
			_ = held.file.Close() // Closing this read-only handle has no pending writes to commit.
		}
		held, r.Memory.Value = nil, nil
	}
	if held != nil && (first || held.inode != read.Inode) {
		// The state that the run before left does not belong to this open
		// file: MMWatchdog did not accept that run.
		forget()
	}
	current, about, err := primitives.OpenRegularFile(l.LogFilePath)
	remaining := logLinesReadLimit
	problem := ""
	continues := err == nil && about.Inode == read.Inode && about.SizeBytes >= read.Offset && startsAsBefore(current, read)
	if !first && l.FailWhenNoNewLineFor.Value() > 0 {
		// Looking at the current file must not wait for the old backlog.
		if err == nil {
			from := int64(0)
			if continues {
				from = read.Offset
			}
			newest, readErr := newestCompleteLogLineAt(ctx, current, about, from, false)
			if readErr != nil {
				_ = current.Close()
				return "MMWatchdog cannot read the newest complete line of " + l.LogFilePath + ": " + readErr.Error()
			}
			fresh.sawLineAt(newest)
		}
		if held != nil && (err != nil || held.inode != about.Inode) {
			heldInfo, readErr := held.file.Stat()
			if readErr == nil {
				var newest time.Time
				newest, readErr = newestCompleteLogLineAt(ctx, held.file, primitives.FileInformationFrom(heldInfo), read.Offset, true)
				fresh.sawLineAt(newest)
			}
			if readErr != nil {
				if current != nil {
					_ = current.Close()
				}
				return "MMWatchdog cannot read the newest complete line of the rotated " + l.LogFilePath + ": " + readErr.Error()
			}
		}
	}
	if held != nil {
		heldNow, heldErr := held.file.Stat()
		if heldErr == nil && (err != nil || !sameOpenFile(heldNow, about)) {
			// The name no longer holds the file of the run before: the
			// program started a new file, or took the old one away. The
			// old file is finished. Read it to its end, with a last line
			// that has no end of line, and then let it go.
			if heldNow.Size() >= read.Offset {
				before := fresh.count
				more, readProblem := readLogLines(ctx, held.file, read, heldNow.Size(), &remaining, true, line)
				if readProblem != "" {
					problem = "MMWatchdog cannot read " + l.LogFilePath + ": " + readProblem
				}
				if fresh.count > before {
					fresh.sawLineAt(heldNow.ModTime().UTC())
				}
				if more {
					// The next run continues in the old file.
					fresh.more = true
					if err == nil {
						_ = current.Close() // Closing this read-only handle has no pending writes to commit.
					}
					return problem
				}
			}
			forget()
			read.Inode, read.Offset, read.StartLength, read.StartChecksum = 0, 0, 0, ""
			read.SkippingOversizedLine = false
		}
	}
	switch {
	case errors.Is(err, fs.ErrNotExist):
		if fresh.count > 0 {
			return ""
		}
		return l.LogFilePath + " does not exist"
	case errors.Is(err, primitives.ErrNotARegularFile):
		return l.LogFilePath + " is " + about.Kind + ", not a regular file"
	case err != nil:
		return "MMWatchdog cannot look at " + l.LogFilePath + ": " + err.Error()
	}
	keep := func() {
		forget()
		if r.Diagnostic {
			// A run of "mmwatchdog check-now" leaves nothing behind.
			_ = current.Close() // Closing this read-only handle has no pending writes to commit.
			return
		}
		r.Memory.Value = &logFileHeld{file: current, inode: about.Inode}
	}
	if first {
		read.Inode, read.Offset = about.Inode, about.SizeBytes
		read.StartLength, read.StartChecksum = startOfLogFile(current)
		keep()
		return ""
	}
	// The file of the run before, when it only grew, is read on from where
	// that run stopped. Everything else is read from its start.
	if !continues {
		read.Inode, read.Offset = about.Inode, 0
		read.SkippingOversizedLine = false
	}
	before := fresh.count
	more, readProblem := readLogLines(ctx, current, read, about.SizeBytes, &remaining, false, line)
	fresh.more = more
	if fresh.count > before {
		fresh.sawLineAt(about.Modified.UTC())
	}
	read.StartLength, read.StartChecksum = startOfLogFile(current)
	keep()
	if readProblem != "" {
		problem = "MMWatchdog cannot read " + l.LogFilePath + ": " + readProblem
	}
	return problem
}

func (fresh *newLogLines) sawLineAt(at time.Time) {
	if at.After(fresh.newestLineAt) {
		fresh.newestLineAt = at.UTC()
	}
}

// newestCompleteLogLineAt looks backward for a complete line without consuming
// any backlog. Files have no per-line timestamp, so their modification time is
// the evidence. A rotated file's final line also counts without a newline.
func newestCompleteLogLineAt(ctx context.Context, file *os.File, about primitives.FileInformation, from int64, finished bool) (time.Time, error) {
	for end := about.SizeBytes; end > from; {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		start := max(from, end-logLineLimit)
		part, err := primitives.ReadFileFrom(file, start, int(end-start))
		if err != nil {
			return time.Time{}, err
		}
		if int64(len(part)) != end-start {
			return time.Time{}, io.ErrUnexpectedEOF
		}
		if finished || bytes.IndexByte(part, '\n') >= 0 {
			return about.Modified, nil
		}
		end = start
	}
	return time.Time{}, nil
}

// sameOpenFile reports whether an open file is the file that the node
// describes in about.
func sameOpenFile(open os.FileInfo, about primitives.FileInformation) bool {
	opened := primitives.FileInformationFrom(open)
	return opened.Inode == about.Inode && opened.Device == about.Device
}

// startOfLogFile returns the length and the SHA-256 checksum of the first
// bytes of a log file, at most logFileStartLimit of them.
func startOfLogFile(f *os.File) (int, string) {
	start, err := primitives.ReadFileFrom(f, 0, logFileStartLimit)
	if err != nil || len(start) == 0 {
		return 0, ""
	}
	sum := sha256.Sum256(start)
	return len(start), hex.EncodeToString(sum[:])
}

// startsAsBefore reports whether a log file still starts with the bytes
// that the run before found at its start. A program adds to the end of its
// log, so another start is another content under the same name: a file that
// was emptied and has grown again, or a new file that got the inode of the
// old one.
func startsAsBefore(f *os.File, read *LogLinesRead) bool {
	if read.StartLength == 0 {
		return true
	}
	start, err := primitives.ReadFileFrom(f, 0, read.StartLength)
	if err != nil || len(start) != read.StartLength {
		return false
	}
	sum := sha256.Sum256(start)
	return hex.EncodeToString(sum[:]) == read.StartChecksum
}

// readLogLines reads the lines of an open log file from offset to end, and
// gives each to line. It reads at most remaining bytes, which it counts
// down, and stops when ctx ends; more says that it stopped before end. A
// line counts once the program has written its end. In a finished file the
// last line counts without one.
func readLogLines(ctx context.Context, file *os.File, read *LogLinesRead, end int64, remaining *int, finished bool, line func(string) bool) (bool, string) {
	problem := ""
	oversized := func(offset int64) {
		if problem == "" {
			problem = fmt.Sprintf("the line at byte %d exceeds 64 KiB; its first 64 KiB did not match, and MMWatchdog skips the rest of that line", offset)
		}
	}
	for read.Offset < end {
		if ctx.Err() != nil || *remaining <= 0 {
			return true, problem
		}
		wanted := int(min(int64(*remaining), logLinesPartLimit, end-read.Offset))
		part, err := primitives.ReadFileFrom(file, read.Offset, wanted)
		if err == nil && len(part) < wanted {
			err = io.ErrUnexpectedEOF
		}
		if err != nil && part != nil {
			// A read may return complete lines and an error together.
			// Report those matches before retrying the unread line.
			part = part[:bytes.LastIndexByte(part, '\n')+1]
		}
		for len(part) > 0 {
			length := bytes.IndexByte(part, '\n')
			next := length + 1
			if read.SkippingOversizedLine {
				if length < 0 {
					next = len(part)
				}
				read.Offset += int64(next)
				*remaining -= next
				part = part[next:]
				if length >= 0 || (finished && read.Offset == end) {
					read.SkippingOversizedLine = false
				}
				continue
			}
			if length < 0 {
				if len(part) <= logLineLimit && !(finished && read.Offset+int64(len(part)) == end) {
					// A short incomplete line stays for the next run, or
					// is reread across this chunk's boundary.
					if read.Offset+int64(len(part)) < end && *remaining > len(part) {
						break
					}
					return read.Offset+int64(len(part)) < end, problem
				}
				length, next = len(part), len(part)
				if length > logLineLimit {
					read.SkippingOversizedLine = true
				}
			}
			matched := line(strings.TrimSuffix(string(part[:min(length, logLineLimit)]), "\r"))
			if length > logLineLimit && !matched {
				oversized(read.Offset)
			}
			part = part[next:]
			read.Offset += int64(next)
			*remaining -= next
			if finished && read.Offset == end {
				read.SkippingOversizedLine = false
			}
		}
		if err != nil {
			message := fmt.Sprintf("read error at byte %d: %s", read.Offset, err)
			if problem != "" {
				message = problem + "; " + message
			}
			return true, message
		}
	}
	return false, problem
}

// journalReadLimit is the most that one run reads of what journalctl
// writes: for each entry its message, its place, and its time.
const journalReadLimit = 8 * 1024 * 1024

// readJournal reads the entries that the unit wrote to the journal since
// the run before, gives the message of each to line, and moves read to the
// last of them. It returns what kept it from reading, or "".
func (l *LogLines) readJournal(ctx context.Context, read *LogLinesRead, first bool, fresh *newLogLines, line func(string) bool) string {
	if _, err := primitives.ServiceProperties(ctx, l.JournalOfSystemdUnit, "ActiveState"); err != nil {
		return err.Error()
	}
	// journalctl writes the message of each entry and the place and the
	// time of the entry, and no other field. --all writes a long message
	// whole: without it journalctl writes null for a message of 4096 bytes
	// or more.
	question := []string{"journalctl", "--no-pager", "--quiet", "--output=json", "--all", "--output-fields=MESSAGE", "--unit=" + l.JournalOfSystemdUnit}
	if first {
		// The place of the newest entry now. A unit that has written
		// nothing yet has none, and is then read from the time of this run.
		newest, err := primitives.RunSystemProgram(ctx, append(question, "--lines=1")...)
		if err != nil {
			return "MMWatchdog cannot read the journal of " + l.JournalOfSystemdUnit + ": " + err.Error()
		}
		for _, entry := range strings.Split(newest, "\n") {
			var written struct {
				Cursor string `json:"__CURSOR"`
			}
			if json.Unmarshal([]byte(entry), &written) == nil && written.Cursor != "" {
				read.Cursor = written.Cursor
			}
		}
		return ""
	}
	from := "--since=@" + strconv.FormatInt(read.WatchingSince.Unix(), 10)
	if read.Cursor != "" {
		from = "--after-cursor=" + read.Cursor
	}
	// A unit can have written more than one run reads. MMWatchdog then ends
	// journalctl at the limit, and the next run continues after the last
	// entry that this one read.
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: append(question, from), AsMMWatchdogItself: true, StandardOutputLimit: journalReadLimit, EndsAtStandardOutputLimit: true})
	problem := ""
	fresh.more = errors.Is(err, primitives.ErrStandardOutputLimit) || (err != nil && ctx.Err() != nil)
	if err != nil && !fresh.more {
		problem = "MMWatchdog cannot read the journal of " + l.JournalOfSystemdUnit + ": " + err.Error()
	}
	if err == nil && ran.ExitCode != 0 {
		problem = "MMWatchdog cannot read the journal of " + l.JournalOfSystemdUnit + ": " + primitives.Clip(strings.TrimSpace(ran.StandardError), 512)
	}
	written := ran.StandardOutput
	if fresh.more {
		// The last line is cut off in the middle.
		written = written[:strings.LastIndexByte(written, '\n')+1]
	}
	for len(written) > 0 {
		one, rest, _ := strings.Cut(written, "\n")
		written = rest
		var entry struct {
			Message   any    `json:"MESSAGE"`
			Cursor    string `json:"__CURSOR"`
			Timestamp string `json:"__REALTIME_TIMESTAMP"`
		}
		if json.Unmarshal([]byte(one), &entry) != nil || entry.Cursor == "" {
			continue
		}
		message := journalMessage(entry.Message)
		read.Cursor = entry.Cursor
		if microseconds, err := strconv.ParseInt(entry.Timestamp, 10, 64); err == nil {
			fresh.newestLineAt = time.UnixMicro(microseconds).UTC()
		}
		matched := line(message[:min(len(message), logLineLimit)])
		if len(message) > logLineLimit && !matched && problem == "" {
			problem = "the journal of " + l.JournalOfSystemdUnit + " entry at cursor " + entry.Cursor + " exceeds 64 KiB; its first 64 KiB did not match, and MMWatchdog skips the rest of that entry"
		}
	}
	if errors.Is(err, primitives.ErrStandardOutputLimit) && fresh.count == 0 {
		// One entry can be larger than the whole batch budget. Obtain its
		// cursor without MESSAGE, then check only its bounded message prefix.
		return l.readOversizedJournalEntry(ctx, read, fresh, question, from, line)
	}
	if fresh.more && l.FailWhenNoNewLineFor.Value() > 0 {
		newest, err := primitives.RunSystemProgram(ctx, append(question, "--lines=1")...)
		if err != nil {
			return "MMWatchdog cannot read the newest line of the journal of " + l.JournalOfSystemdUnit + ": " + err.Error()
		}
		for _, one := range strings.Split(newest, "\n") {
			var entry struct {
				Message   any    `json:"MESSAGE"`
				Timestamp string `json:"__REALTIME_TIMESTAMP"`
			}
			if json.Unmarshal([]byte(one), &entry) != nil {
				continue
			}
			if len(journalMessage(entry.Message)) > logLineLimit {
				return "the newest journal line of " + l.JournalOfSystemdUnit + " exceeds 64 KiB"
			}
			microseconds, err := strconv.ParseInt(entry.Timestamp, 10, 64)
			if err != nil {
				return "the newest journal line of " + l.JournalOfSystemdUnit + " has no valid timestamp"
			}
			fresh.newestLineAt = time.UnixMicro(microseconds).UTC()
		}
	}
	return problem
}

// readOversizedJournalEntry advances one entry that cannot fit the batch
// output budget. Journal metadata has its own query so MESSAGE cannot hide
// the cursor behind an arbitrarily long JSON string.
func (l *LogLines) readOversizedJournalEntry(ctx context.Context, read *LogLinesRead, fresh *newLogLines, question []string, from string, line func(string) bool) string {
	metadataQuestion := append([]string{}, question...)
	for index, argument := range metadataQuestion {
		if strings.HasPrefix(argument, "--output-fields=") {
			metadataQuestion[index] = "--output-fields=__CURSOR"
		}
	}
	metadata, err := primitives.RunCommand(ctx, primitives.Command{Arguments: append(metadataQuestion, from), AsMMWatchdogItself: true, StandardOutputLimit: logLineLimit, EndsAtStandardOutputLimit: true})
	source := "the journal of " + l.JournalOfSystemdUnit
	if err != nil && !errors.Is(err, primitives.ErrStandardOutputLimit) {
		return "MMWatchdog cannot read the next cursor of " + source + ": " + err.Error()
	}
	if err == nil && metadata.ExitCode != 0 {
		return "MMWatchdog cannot read the next cursor of " + source + ": " + primitives.DetailLine(metadata.StandardError)
	}
	first, _, _ := strings.Cut(metadata.StandardOutput, "\n")
	var entry struct {
		Cursor    string `json:"__CURSOR"`
		Timestamp string `json:"__REALTIME_TIMESTAMP"`
	}
	if json.Unmarshal([]byte(first), &entry) != nil || entry.Cursor == "" {
		return "MMWatchdog cannot read the next cursor of " + source + ": journalctl returned no complete entry metadata"
	}
	prefix, err := primitives.RunCommand(ctx, primitives.Command{Arguments: []string{"journalctl", "--no-pager", "--quiet", "--output=cat", "--all", "--unit=" + l.JournalOfSystemdUnit, "--cursor=" + entry.Cursor}, AsMMWatchdogItself: true, StandardOutputLimit: logLineLimit, EndsAtStandardOutputLimit: true})
	matched := false
	if prefix.StandardOutput != "" {
		matched = line(prefix.StandardOutput)
	}
	if err != nil && !errors.Is(err, primitives.ErrStandardOutputLimit) {
		return "MMWatchdog cannot read entry at cursor " + entry.Cursor + " of " + source + ": " + err.Error()
	}
	if err == nil && prefix.ExitCode != 0 {
		return "MMWatchdog cannot read entry at cursor " + entry.Cursor + " of " + source + ": " + primitives.DetailLine(prefix.StandardError)
	}
	if len(prefix.StandardOutput) < logLineLimit {
		return "MMWatchdog cannot read the complete 64 KiB prefix at cursor " + entry.Cursor + " of " + source
	}
	read.Cursor = entry.Cursor
	if microseconds, err := strconv.ParseInt(entry.Timestamp, 10, 64); err == nil {
		fresh.sawLineAt(time.UnixMicro(microseconds))
	}
	if !matched {
		return source + " entry at cursor " + entry.Cursor + " exceeds 64 KiB; its first 64 KiB did not match, and MMWatchdog skips the rest of that entry"
	}
	return ""
}

// journalMessage is the text of a journal entry. journalctl writes a
// message that is no valid text as a list of the numbers of its bytes.
func journalMessage(message any) string {
	switch value := message.(type) {
	case string:
		return value
	case []any:
		text := []byte{}
		for _, number := range value {
			if b, isNumber := number.(float64); isNumber {
				text = append(text, byte(b))
			}
		}
		return strings.ToValidUTF8(string(text), "�")
	}
	return ""
}

// logLinesDescription is what "mmwatchdog describe-module log_lines" shows, and what
// validation knows about the module's inputs.
var logLinesDescription = ModuleDescription{
	Summary: "Reads the lines that a program adds to a log, from a file or from the journal of a systemd unit. A run reads only the lines that are new since the run before. The check states one source and one condition.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "log_file_path", Type: "string", Description: "Source: the absolute path of the log file that the lines are added to."},
		{Name: "journal_of_systemd_unit", Type: "string", Description: "Source: the systemd unit whose lines in the journal the check reads, named with its kind, such as myapp.service or backup.timer."},
		{Name: "fail_when_a_new_line_contains", Type: "string", Description: "Condition: a run fails when at least one new line contains this text."},
		{Name: "fail_when_a_new_line_matches_regex", Type: "string", Description: "Condition: a run fails when at least one new line matches this regular expression."},
		{Name: "fail_when_no_new_line_for", Type: "duration", Description: "Condition: a run fails when the source got no new line for this long."},
	},
	Examples: []ModuleExample{
		{
			Description:   "Alert when the application writes a line with FATAL to its log file.",
			Configuration: "log_lines:\n  log_file_path: /var/log/myapp/application.log\n  fail_when_a_new_line_contains: \"FATAL\"\nalert_severity: warn\n",
		},
		{
			Description:   "Alert when the worker has written nothing to the journal for 10 minutes.",
			Configuration: "log_lines:\n  journal_of_systemd_unit: myapp-worker.service\n  fail_when_no_new_line_for: 10m\nalert_severity: sos\n",
		},
	},
}
