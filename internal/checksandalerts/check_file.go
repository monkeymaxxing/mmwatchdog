package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// File is the inputs of the file check module, which looks at one file or
// directory: whether it exists, who owns it, what its permissions are, how
// old it is, and for a regular file its content and how large it is. The
// check fails when nothing is at the path, unless MustNotExist turns that
// around.
type File struct {
	Path                    string `yaml:"path" json:"path"`
	MustNotExist            bool   `yaml:"must_not_exist" json:"must_not_exist,omitempty"`
	ExpectedOwner           string `yaml:"expected_owner" json:"expected_owner,omitempty"`
	ExpectedGroup           string `yaml:"expected_group" json:"expected_group,omitempty"`
	ExpectedPermissionsMode string `yaml:"expected_permissions_mode" json:"expected_permissions_mode,omitempty"`
	ExpectedSHA256Checksum  string `yaml:"expected_sha256_checksum" json:"expected_sha256_checksum,omitempty"`
}

var (
	permissionsModePattern = regexp.MustCompile(`^[0-7]{3,4}$`)
	sha256ChecksumPattern  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (f *File) Validate(v InputValidation) *FieldError {
	if !filepath.IsAbs(f.Path) || filepath.Clean(f.Path) != f.Path {
		return &FieldError{"path", "is required: the absolute path of the file or directory that the check looks at"}
	}
	expected := map[string]string{"expected_owner": f.ExpectedOwner, "expected_group": f.ExpectedGroup, "expected_permissions_mode": f.ExpectedPermissionsMode, "expected_sha256_checksum": f.ExpectedSHA256Checksum}
	for _, name := range Names(expected) {
		switch {
		case !v.Authored(name):
		case f.MustNotExist:
			return &FieldError{name, "a check with must_not_exist: true looks only at whether the file exists; remove " + name}
		case strings.TrimSpace(expected[name]) == "" || strings.ContainsAny(expected[name], " \t\r\n"):
			return &FieldError{name, "cannot be empty and holds no white space"}
		}
	}
	if f.ExpectedPermissionsMode != "" && !permissionsModePattern.MatchString(f.ExpectedPermissionsMode) {
		return &FieldError{"expected_permissions_mode", "expected the permissions as chmod writes them, in quotes, such as \"0600\""}
	}
	if f.ExpectedSHA256Checksum != "" && !sha256ChecksumPattern.MatchString(f.ExpectedSHA256Checksum) {
		return &FieldError{"expected_sha256_checksum", "expected a SHA-256 checksum: 64 hex characters in lower case, as sha256sum prints them"}
	}
	return nil
}

// ValidateCheck refuses a limit on a check that says only that its file
// must not exist: a file that is not there has no age and no size.
func (f *File) ValidateCheck(c Check) *FieldError {
	if !f.MustNotExist {
		return nil
	}
	for _, field := range LimitSettings {
		if c.Limits(field) != nil {
			return &FieldError{field, "a file check with must_not_exist: true looks only at whether the file exists; remove " + field}
		}
	}
	return nil
}

func (f *File) Run(ctx context.Context, r *CheckRun) Observation {
	about, err := primitives.InformationAboutFile(f.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist) && f.MustNotExist:
		return Result("ok", f.Path+" does not exist")
	case errors.Is(err, fs.ErrNotExist):
		return Result("failed", f.Path+" does not exist")
	case err != nil:
		return Result("unknown", "MMWatchdog cannot look at "+f.Path+": "+err.Error())
	case f.MustNotExist:
		return Result("failed", f.Path+" exists")
	case !about.IsRegularFile && !about.IsDirectory:
		return Result("failed", f.Path+" is "+about.Kind+", not a regular file or a directory")
	case about.IsDirectory && f.ExpectedSHA256Checksum != "":
		return Result("failed", f.Path+" is a directory, and expected_sha256_checksum checks the content of a regular file")
	}
	// A directory has no size of its content, so a limit on it cannot hold.
	for _, field := range LimitSettings {
		if _, limited := r.Check.Limits(field)["file_size_mib"]; limited && about.IsDirectory {
			return Result("failed", f.Path+" is a directory, and the limit on file_size_mib in "+field+" checks the size of a regular file")
		}
	}
	// A check of the content opens the file once, and takes the owner, the
	// permissions, the size, and the content from that one open file.
	checksum := ""
	if f.ExpectedSHA256Checksum != "" {
		opened, aboutOpened, err := primitives.OpenRegularFile(f.Path)
		switch {
		case errors.Is(err, primitives.ErrNotARegularFile):
			return Result("failed", f.Path+" is "+aboutOpened.Kind+", not a regular file")
		case errors.Is(err, fs.ErrNotExist):
			return Result("failed", f.Path+" does not exist")
		case err != nil:
			return Result("unknown", "MMWatchdog cannot read "+f.Path+": "+err.Error())
		}
		defer func() {
			_ = opened.Close() // Closing this read-only handle has no pending writes to commit.
		}()
		about = aboutOpened
		checksum, err = primitives.Sha256ChecksumOfFile(ctx, opened)
		switch {
		case ctx.Err() != nil:
			return Result("unknown", "MMWatchdog could not read all of "+f.Path+" within the time limit of the run")
		case err != nil:
			return Result("unknown", "MMWatchdog cannot read "+f.Path+": "+err.Error())
		}
	}
	// Each expected property that differs is named with what the file has.
	differs := []string{}
	if f.ExpectedOwner != "" && f.ExpectedOwner != strconv.FormatUint(uint64(about.UserID), 10) {
		if owner := primitives.UserAccountName(ctx, about.UserID); f.ExpectedOwner != owner {
			differs = append(differs, "is owned by the user account "+owner+", expected "+f.ExpectedOwner)
		}
	}
	if f.ExpectedGroup != "" && f.ExpectedGroup != strconv.FormatUint(uint64(about.GroupID), 10) {
		if group := primitives.GroupName(ctx, about.GroupID); f.ExpectedGroup != group {
			differs = append(differs, "belongs to the group "+group+", expected "+f.ExpectedGroup)
		}
	}
	if f.ExpectedPermissionsMode != "" {
		if expected, _ := strconv.ParseUint(f.ExpectedPermissionsMode, 8, 32); uint32(expected) != about.PermissionsMode {
			differs = append(differs, fmt.Sprintf("has permissions mode %04o, expected %04o", about.PermissionsMode, expected))
		}
	}
	if f.ExpectedSHA256Checksum != "" && checksum != f.ExpectedSHA256Checksum {
		differs = append(differs, "has the SHA-256 checksum "+checksum+", expected "+f.ExpectedSHA256Checksum)
	}
	if len(differs) > 0 {
		return Result("failed", f.Path+" "+strings.Join(differs, "; "+f.Path+" "))
	}
	o := Result("ok", f.Path+" exists")
	if f.ExpectedOwner+f.ExpectedGroup+f.ExpectedPermissionsMode+f.ExpectedSHA256Checksum != "" {
		o.Message = f.Path + " is as expected"
	}
	o.Measurements = map[string]Measurement{}
	if about.IsRegularFile {
		o.Measurements["file_size_mib"] = Measurement{float64(about.SizeBytes) / (1024 * 1024), "MiB"}
	}
	// A file whose content was last changed in the future has no age. A
	// directory's age counts from the last entry that was added, removed,
	// or renamed in it.
	unavailable := map[string]string{}
	age := time.Since(about.Modified)
	for name, unit := range map[string]time.Duration{"file_age_minutes": time.Minute, "file_age_hours": time.Hour, "file_age_days": 24 * time.Hour} {
		if age < 0 {
			unavailable[name] = fmt.Sprintf("%s was last changed at %s, which is in the future, so its age cannot be measured", f.Path, about.Modified.UTC().Format("2006-01-02 15:04 UTC"))
			continue
		}
		o.Measurements[name] = Measurement{float64(age) / float64(unit), strings.TrimPrefix(name, "file_age_")}
	}
	return withLimits(r.Check, o, unavailable)
}

// fileDescription is what "mmwatchdog describe-module file" shows, and what
// validation knows about the module's inputs.
var fileDescription = ModuleDescription{
	Summary: "Looks at one file or directory: whether it exists, who owns it, what its permissions are, how old it is, and for a regular file its content and how large it is. The check fails when nothing is at the path.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "path", Type: "string", Required: true, Description: "The absolute path of the file or directory that the check looks at."},
		{Name: "must_not_exist", Type: "boolean", Description: "With true, the check fails while a file or directory is at the path, and takes no other input and no limit."},
		{Name: "expected_owner", Type: "string", Description: "The user account that must own the file or directory."},
		{Name: "expected_group", Type: "string", Description: "The group that the file or directory must belong to."},
		{Name: "expected_permissions_mode", Type: "string", Description: "The permissions that the file or directory must have, as chmod writes them, in quotes, such as \"0600\"."},
		{Name: "expected_sha256_checksum", Type: "string", Description: "The SHA-256 checksum that the file's content must have: 64 hex characters, as sha256sum prints them. Only for a regular file."},
	},
	Measurements: []ModuleMeasurement{
		{Name: "file_age_minutes", Unit: "minutes", Description: "The time since the file's content was last changed, in minutes. For a directory, the time since an entry was last added, removed, or renamed in it."},
		{Name: "file_age_hours", Unit: "hours", Description: "The same time in hours."},
		{Name: "file_age_days", Unit: "days", Description: "The same time in days."},
		{Name: "file_size_mib", Unit: "MiB", Description: "The size of a regular file. A directory has none."},
	},
	Examples: []ModuleExample{
		{
			Description:   "Fail when the backup file is missing, older than 26 hours, or smaller than 1 MiB.",
			Configuration: "file:\n  path: /var/backups/database.sql.gz\nfail_when_measurement_above:\n  file_age_hours: 26\nfail_when_measurement_below:\n  file_size_mib: 1\nalert_severity: sos\n",
		},
		{
			Description:   "Fail while a file that marks maintenance exists.",
			Configuration: "file:\n  path: /srv/myapp/maintenance.flag\n  must_not_exist: true\nalert_severity: warn\n",
		},
	},
}
