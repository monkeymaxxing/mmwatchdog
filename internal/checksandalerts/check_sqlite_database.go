package checksandalerts

import (
	"context"
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"mmwatchdog/internal/primitives"
)

// SQLiteDatabase is the inputs of the sqlite_database check module, which
// looks at the SQLite database file of another program: that the file is a
// SQLite database, that a small query runs within the time limit, and how
// large the database and its write-ahead log are. CheckIntegrity also
// reads every page. The check opens the file for reading only, as an
// ordinary reader of the database, which in a database with a write-ahead
// log never makes a writer wait. The check runs no query on a database
// without a write-ahead log: such a database is changed in place, and a
// reader would make its writer wait.
type SQLiteDatabase struct {
	Path           string `yaml:"path" json:"path"`
	CheckIntegrity bool   `yaml:"check_integrity" json:"check_integrity,omitempty"`
}

func (s *SQLiteDatabase) Validate(InputValidation) *FieldError {
	if !filepath.IsAbs(s.Path) || filepath.Clean(s.Path) != s.Path {
		return &FieldError{"path", "is required: the absolute path of the SQLite database file"}
	}
	return nil
}

// A SQLite database file starts with this text. The two bytes after its
// first 18 say how the database is written and read: both are 2 for a
// database that keeps a write-ahead log.
const (
	sqliteFileStart     = "SQLite format 3\x00"
	SQLiteHeaderBytes   = 100
	sqliteWriteAheadLog = 2
)

func (s *SQLiteDatabase) Run(ctx context.Context, r *CheckRun) Observation {
	opened, about, err := primitives.OpenRegularFile(s.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Result("failed", s.Path+" does not exist")
	case errors.Is(err, primitives.ErrNotARegularFile):
		return Result("failed", s.Path+" is "+about.Kind+", not a regular file")
	case err != nil:
		return Result("unknown", "MMWatchdog cannot look at "+s.Path+": "+err.Error())
	}
	header, err := primitives.ReadFileFrom(opened, 0, SQLiteHeaderBytes)
	_ = opened.Close() // Closing this read-only handle has no pending writes to commit.
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read "+s.Path+": "+err.Error())
	}
	if len(header) < SQLiteHeaderBytes || !strings.HasPrefix(string(header), sqliteFileStart) {
		return Result("failed", s.Path+" is no SQLite database: the file does not start as a SQLite database does")
	}
	megabytes := func(bytes int64) Measurement { return Measurement{float64(bytes) / (1024 * 1024), "MiB"} }
	measurements := map[string]Measurement{"database_size_mib": megabytes(about.SizeBytes), "write_ahead_log_size_mib": megabytes(0)}
	if log, err := primitives.InformationAboutFile(s.Path + "-wal"); err == nil {
		measurements["write_ahead_log_size_mib"] = megabytes(log.SizeBytes)
	}
	o := Result("ok", "")
	if header[18] != sqliteWriteAheadLog || header[19] != sqliteWriteAheadLog {
		// A database without a write-ahead log is changed in place. A
		// reader with a lock makes the writing program wait, or fail, at
		// its next change, and a reader without one sees half of a change.
		// So MMWatchdog reads the file's first bytes and its size only.
		if s.CheckIntegrity {
			return Result("unknown", s.Path+" keeps no write-ahead log, so MMWatchdog runs no query on it and cannot check its integrity: a reader of such a database makes the program that writes it wait")
		}
		o.Message = s.Path + " is a SQLite database; it keeps no write-ahead log, so MMWatchdog ran no query on it"
	} else {
		// The smallest question that reads the database through SQLite, as
		// an ordinary reader. Such a reader sees one finished state of the
		// database, also while SQLite moves changes from the log into the
		// database file. A reader that skips SQLite's locks (immutable=1)
		// can meet the file in the middle of such a move, and SQLite then
		// calls a healthy database malformed.
		if _, err := primitives.SQLiteQuery(ctx, s.Path, "SELECT count(*) FROM sqlite_schema"); err != nil {
			return s.queryProblem(ctx, "a query", err)
		}
		o.Message = s.Path + " answers a query"
		if s.CheckIntegrity {
			found, err := primitives.SQLiteQuery(ctx, s.Path, "PRAGMA integrity_check(10)")
			if err != nil {
				return s.queryProblem(ctx, "the check of its integrity", err)
			}
			if len(found) != 1 || found[0] != "ok" {
				// The check stops after ten problems.
				count := countOf(len(found), "problem", "problems")
				if len(found) == 10 {
					count = "10 problems or more"
				}
				damaged := Result("failed", s.Path+" is damaged: the check of its integrity found "+count)
				for _, problem := range found[:min(len(found), primitives.DetailLinesLimit)] {
					damaged.Details = append(damaged.Details, primitives.DetailLine(problem))
				}
				return damaged
			}
			o.Message += ", and its integrity is intact"
		}
	}
	o.Measurements = measurements
	return withLimits(r.Check, o, nil)
}

// queryProblem is the observation of a query that did not run to its end.
// Only SQLite's own verdict that the file is damaged, or that it is no
// database, fails the check. Every other error, such as a database that
// stayed locked, a query that the time limit ended, or a file beside the
// database that SQLite cannot open, establishes nothing about the
// database: the check is unknown.
func (s *SQLiteDatabase) queryProblem(ctx context.Context, what string, err error) Observation {
	var sqliteError *sqlite.Error
	errors.As(err, &sqliteError)
	// The lowest byte of SQLite's result code is the primary code, and the
	// byte above it says more.
	primaryCode := -1
	if sqliteError != nil {
		primaryCode = sqliteError.Code() & 0xff
	}
	switch {
	case ctx.Err() != nil:
		return Result("unknown", "MMWatchdog could not finish "+what+" on "+s.Path+" within the time limit of the run")
	case primaryCode == sqlite3.SQLITE_CORRUPT || primaryCode == sqlite3.SQLITE_NOTADB:
		return Result("failed", primitives.DetailLine("SQLite cannot run "+what+" on "+s.Path+": "+err.Error()))
	case primaryCode == sqlite3.SQLITE_BUSY || primaryCode == sqlite3.SQLITE_LOCKED:
		return Result("unknown", "MMWatchdog could not run "+what+" on "+s.Path+": another program holds the database locked")
	}
	return Result("unknown", primitives.DetailLine("MMWatchdog could not run "+what+" on "+s.Path+": "+err.Error()))
}

// sqliteDatabaseDescription is what "mmwatchdog describe-module sqlite_database" shows, and what
// validation knows about the module's inputs.
var sqliteDatabaseDescription = ModuleDescription{
	Summary: "Looks at the SQLite database file of another program: runs a small query on it, and reports the size of the database and of its write-ahead log. The check opens the file for reading only.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "path", Type: "string", Required: true, Description: "The absolute path of the SQLite database file."},
		{Name: "check_integrity", Type: "boolean", Description: "With true, the check also reads every page of the database and fails when SQLite finds damage. On a large database this takes long."},
	},
	Measurements: []ModuleMeasurement{
		{Name: "database_size_mib", Unit: "MiB", Description: "The size of the database file."},
		{Name: "write_ahead_log_size_mib", Unit: "MiB", Description: "The size of the write-ahead log beside the database file, or 0 without one."},
	},
	Examples: []ModuleExample{{
		Description:   "The application's database answers a query, and its write-ahead log stays below 256 MiB.",
		Configuration: "sqlite_database:\n  path: /var/lib/myapp/myapp.db\nfail_when_measurement_above:\n  write_ahead_log_size_mib: 256\nalert_severity: warn\n",
	}},
}
