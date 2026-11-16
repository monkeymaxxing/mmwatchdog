package checksandalerts

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"mmwatchdog/internal/primitives"
)

// A check whose module is not core has a state directory: what the module
// keeps there is still there at the check's next run. The directory is
// MODULE_STATE_DIR/CHECK_GROUP/CHECK, belongs to the account that the
// module runs as, and no other account can read it. An executable gets the
// path in its request, and a script stores one value there with
// write_state. A core module keeps its state with the check, in the
// daemon's own state (ModuleState). The rules are in docs/changing-mmwatchdog/design/checks.md
// under "State kept between runs".

// The daemon runs as root, and the account of a module controls everything
// inside a state directory. So the daemon never follows a path into such a
// directory with root's rights, except through os.Root, which refuses a path
// that leaves the directory through a symbolic link. Without that rule an
// account could put a link in the place of a directory and have root read
// or give away a file elsewhere on the node.
//
// The directories above a state directory are MMWatchdog's own: MMWatchdog makes them,
// they belong to the account that MMWatchdog runs as, and no other account can
// change them. MMWatchdog checks that before it works in one (mmwatchdogsOwnDirectory), so
// a module_state_dir that names, by mistake, a directory with other content
// is refused, and MMWatchdog changes and removes nothing there.

// CheckNowStateDirectory holds, below the node's module_state_dir, the
// copies of state directories that runs of "mmwatchdog check-now" work on.
const CheckNowStateDirectory = ".check-now"

// CheckNowStateCopyLimit is the most bytes of files that a run of "mmwatchdog
// check-now" copies. The module's account decides how much its state
// directory holds, and a file with a hole can read as far more than it
// takes on the disk. Without a limit that account could have root fill the
// disk with one copy.
const CheckNowStateCopyLimit = 64 * 1024 * 1024

var errStateDirectoryLargerThanCopyLimit = errors.New("the module state directory exceeds the 64 MiB limit for a copy made by mmwatchdog check-now")

// CheckNowStateCopyEntryLimit is the most files and directories that a run
// of "mmwatchdog check-now" copies. A directory with millions of empty files holds
// no bytes to count, and its copy would keep MMWatchdog busy until the time limit.
const CheckNowStateCopyEntryLimit = 10_000

var errStateDirectoryHoldsMoreEntriesThanCopyLimit = errors.New("the module state directory exceeds the limit of 10000 files and directories for a copy made by mmwatchdog check-now")

// RemovedStateDirectory holds, below the node's module_state_dir, the state
// directories of checks that are gone, from the moment MMWatchdog takes them away
// until it has removed them.
const RemovedStateDirectory = ".removed"

// ModuleStateDirectory prepares the state directory of the run's check for a
// module that runs as account, and returns its path. A scheduled run gets
// the check's own directory. A run of "mmwatchdog check-now" gets a copy, which done
// removes: looking at a check never changes what its next scheduled run
// sees. A run that belongs to no check of a node gets an empty directory,
// which done removes too.
func (r *CheckRun) ModuleStateDirectory(ctx context.Context, account string) (directory string, done func(), e error) {
	nothing := func() {}
	owner, e := stateDirectoryOwnerFor(account)
	if e != nil {
		return "", nothing, e
	}
	if r.ModuleStateDir == "" || r.CheckGroup == "" || r.CheckName == "" {
		if directory, e = os.MkdirTemp("", "mmwatchdog-module-state-"); e != nil {
			return "", nothing, e
		}
		return directory, func() { os.RemoveAll(directory) }, owner.give(directory)
	}
	if e := ModuleStateDirProblem(r.ModuleStateDir); e != nil {
		return "", nothing, e
	}
	own := filepath.Join(r.ModuleStateDir, r.CheckGroup, r.CheckName)
	if !r.Diagnostic {
		// Every account can pass through the two directories above the
		// check's own, and only the module's account can enter that one.
		for _, above := range []string{r.ModuleStateDir, filepath.Dir(own)} {
			if e := MakeMMWatchdogsOwnDirectory(above, 0711); e != nil {
				return "", nothing, e
			}
		}
		if e := os.Mkdir(own, 0700); e != nil && !errors.Is(e, fs.ErrExist) {
			return "", nothing, e
		}
		if e := owner.madeByMMWatchdog(own); e != nil {
			return "", nothing, e
		}
		return own, nothing, owner.give(own)
	}
	copies := filepath.Join(r.ModuleStateDir, CheckNowStateDirectory)
	for _, above := range []string{r.ModuleStateDir, copies} {
		if e := MakeMMWatchdogsOwnDirectory(above, 0711); e != nil {
			return "", nothing, e
		}
	}
	if directory, e = os.MkdirTemp(copies, r.CheckGroup+"-"+r.CheckName+"-"); e != nil {
		return "", nothing, e
	}
	done = func() { os.RemoveAll(directory) }
	// The copy stays root's own, closed to every other account, until all of
	// it is there. Only then does the module's account get the directory.
	if e := CopyStateDirectory(ctx, own, directory, owner); e != nil {
		if ctx.Err() != nil {
			e = errors.New("the copy of the state directory for mmwatchdog check-now took longer than the time limit of the check")
		}
		return directory, done, e
	}
	return directory, done, owner.give(directory)
}

// ModuleStateDirProblem says what is wrong with a module_state_dir, or nil.
// MMWatchdog makes the directory itself, in a directory that is there and that only
// root can change. A directory that is there already must be one that MMWatchdog
// made.
func ModuleStateDirProblem(base string) error {
	above := filepath.Dir(base)
	info, e := os.Stat(above)
	if e != nil || !info.IsDir() {
		return fmt.Errorf("%s must be an existing directory, so that MMWatchdog can create %s in that directory", above, base)
	}
	if info.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("%s has the mode %04o, so other accounts can change what is in it: MMWatchdog keeps the state of modules below a directory that only its owner can change", above, info.Mode().Perm())
	}
	if _, e := os.Lstat(base); errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	return NotMMWatchdogsOwnDirectory(base, 0711)
}

// MakeMMWatchdogsOwnDirectory makes one of the directories that MMWatchdog keeps for itself
// above the state directories, with this mode: 0711 for a directory that
// every account passes through and only root lists. When the directory is
// there already, it must be one that MMWatchdog made. MMWatchdog never changes the mode of
// a directory that it did not make.
func MakeMMWatchdogsOwnDirectory(directory string, mode os.FileMode) error {
	e := os.Mkdir(directory, mode)
	if errors.Is(e, fs.ErrExist) {
		return NotMMWatchdogsOwnDirectory(directory, mode)
	}
	if e != nil {
		return e
	}
	// The mode of the new directory does not depend on the daemon's umask.
	return os.Chmod(directory, mode)
}

// NotMMWatchdogsOwnDirectory says why a directory is not one that MMWatchdog made for the
// state of modules, or nil: it is a real directory and no link, it belongs
// to the account that MMWatchdog runs as, and it has the mode that MMWatchdog gives it.
func NotMMWatchdogsOwnDirectory(directory string, mode os.FileMode) error {
	info, e := os.Lstat(directory)
	if e != nil {
		return e
	}
	why := ""
	switch stat, known := info.Sys().(*syscall.Stat_t); {
	case !info.IsDir():
		why = "the path is not a directory"
	case !known || int(stat.Uid) != os.Geteuid():
		why = "the owner is not the account running MMWatchdog"
	case info.Mode().Perm() != mode:
		why = fmt.Sprintf("the permissions mode is %04o; MMWatchdog requires %04o", info.Mode().Perm(), mode)
	default:
		return nil
	}
	return fmt.Errorf("%s cannot be used for module state: %s. MMWatchdog creates module_state_dir and the directories below module_state_dir; choose a path for module_state_dir where no file or directory exists", directory, why)
}

// StateDirectoryOwner is the account that a state directory belongs to. MMWatchdog
// can give a directory to another account only as root. A process that is
// not root, such as a test, runs its modules as itself, and its state
// directories stay its own.
type StateDirectoryOwner struct {
	UserID, GroupID int
	// IsAnotherAccount reports that MMWatchdog is root and gives the directory away.
	IsAnotherAccount bool
}

func stateDirectoryOwnerFor(account string) (StateDirectoryOwner, error) {
	if os.Geteuid() != 0 {
		return StateDirectoryOwner{}, nil
	}
	u, e := primitives.LookUpUserAccount(account)
	if e != nil {
		return StateDirectoryOwner{}, e
	}
	userID, e := primitives.ParseID(u.Uid)
	if e != nil {
		return StateDirectoryOwner{}, e
	}
	groupID, e := primitives.ParseID(u.Gid)
	if e != nil {
		return StateDirectoryOwner{}, e
	}
	return StateDirectoryOwner{int(userID), int(groupID), true}, nil
}

// madeByMMWatchdog says why MMWatchdog does not use a state directory that is there, or nil.
// MMWatchdog made the directory, so it is a real directory and no link, and it
// belongs to the module's account, or still to MMWatchdog when MMWatchdog ended between
// the two steps of making it.
func (o StateDirectoryOwner) madeByMMWatchdog(directory string) error {
	info, e := os.Lstat(directory)
	if e != nil {
		return e
	}
	stat, known := info.Sys().(*syscall.Stat_t)
	if info.IsDir() && known && (int(stat.Uid) == os.Geteuid() || o.IsAnotherAccount && int(stat.Uid) == o.UserID) {
		return nil
	}
	return fmt.Errorf("%s is not a state directory created by MMWatchdog for the check and the check's account; remove the path so MMWatchdog can create the state directory again", directory)
}

// owns reports a file that belongs to the account. A process that is not
// root gives nothing away, so every file that it can read counts.
func (o StateDirectoryOwner) owns(file os.FileInfo) bool {
	if !o.IsAnotherAccount {
		return true
	}
	stat, known := file.Sys().(*syscall.Stat_t)
	return known && int(stat.Uid) == o.UserID
}

// give makes one directory the account's own, with access for that account
// alone. It changes the directory itself and nothing inside it: what is
// inside is the account's already, and MMWatchdog does not follow paths there.
func (o StateDirectoryOwner) give(directory string) error {
	if e := os.Chmod(directory, 0700); e != nil {
		return e
	}
	if !o.IsAnotherAccount {
		return nil
	}
	return os.Lchown(directory, o.UserID, o.GroupID)
}

// CopyStateDirectory copies the directories and the regular files of the
// state directory from into the directory to, which exists and is empty,
// and gives each copied entry to the owner. A from that does not exist
// copies nothing. Links and special files are not copied. The account of
// the module can change from while the copy runs, so every path in it is
// opened through os.Root, and a file is read only after MMWatchdog has seen, on the
// open file, that it is a regular file. The copy ends with ctx, and with an
// error when the files hold more than CheckNowStateCopyLimit bytes.
func CopyStateDirectory(ctx context.Context, from, to string, owner StateDirectoryOwner) error {
	source, e := os.OpenRoot(from)
	if errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	defer source.Close()
	target, e := os.OpenRoot(to)
	if e != nil {
		return e
	}
	defer target.Close()
	bytesLeft, entriesLeft := int64(CheckNowStateCopyLimit), CheckNowStateCopyEntryLimit
	return fs.WalkDir(source.FS(), ".", func(path string, entry fs.DirEntry, e error) error {
		if e != nil || path == "." {
			return e
		}
		if e := ctx.Err(); e != nil {
			return e
		}
		if entriesLeft--; entriesLeft < 0 {
			return errStateDirectoryHoldsMoreEntriesThanCopyLimit
		}
		switch {
		case entry.IsDir():
			if e := target.Mkdir(path, 0700); e != nil {
				return e
			}
		case !entry.Type().IsRegular():
			return nil
		default:
			if copied, e := copyRegularFile(ctx, source, target, path, owner, &bytesLeft); e != nil || !copied {
				return e
			}
		}
		if !owner.IsAnotherAccount {
			return nil
		}
		return target.Lchown(path, owner.UserID, owner.GroupID)
	})
}

// copyRegularFile copies one file between two directory trees. It reports
// false, and copies nothing, for a path that is no regular file any more
// when MMWatchdog opens it, and for a file that does not belong to the owner of the
// state directory. The open does not wait: a named pipe in the place of
// the file would otherwise hold MMWatchdog until some program writes to it. The
// copy takes its bytes from bytesLeft and ends with an error when none are
// left, and it ends with ctx.
func copyRegularFile(ctx context.Context, source, target *os.Root, path string, owner StateDirectoryOwner, bytesLeft *int64) (bool, error) {
	in, e := source.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if e != nil {
		return false, e
	}
	defer func() {
		_ = in.Close() // Closing this read-only handle has no pending writes to commit.
	}()
	info, e := in.Stat()
	if e != nil || !info.Mode().IsRegular() {
		return false, e
	}
	// A file of another account is not the state of the module: the
	// module's account can only have put a hard link to that file there.
	// The copy becomes the account's own, so MMWatchdog leaves the file out, and
	// the account never gets the content of a file that it could not read.
	if !owner.owns(info) {
		return false, nil
	}
	out, e := target.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, info.Mode().Perm())
	if e != nil {
		return false, e
	}
	for {
		if e := ctx.Err(); e != nil {
			_ = out.Close() // The copy already failed and is discarded; closing cannot turn it into an accepted copy.
			return false, e
		}
		// One byte more than the limit allows shows that the files hold more.
		copied, e := io.CopyN(out, in, min(*bytesLeft+1, 1024*1024))
		if *bytesLeft -= copied; *bytesLeft < 0 {
			_ = out.Close() // The copy already failed and is discarded; closing cannot turn it into an accepted copy.
			return false, errStateDirectoryLargerThanCopyLimit
		}
		if errors.Is(e, io.EOF) {
			return true, out.Close()
		}
		if e != nil {
			_ = out.Close() // The copy already failed and is discarded; closing cannot turn it into an accepted copy.
			return false, e
		}
	}
}

// RemoveLeftoverModuleState removes what an earlier run of MMWatchdog left below
// module_state_dir for a moment only: the copies of runs of "mmwatchdog check-now",
// and the state directories that it took away and did not get to remove. A
// start of MMWatchdog does this, when nothing else works there.
func RemoveLeftoverModuleState(base string) error {
	if _, e := os.Lstat(base); errors.Is(e, fs.ErrNotExist) {
		return nil
	}
	if e := NotMMWatchdogsOwnDirectory(base, 0711); e != nil {
		return e
	}
	return errors.Join(os.RemoveAll(filepath.Join(base, CheckNowStateDirectory)), os.RemoveAll(filepath.Join(base, RemovedStateDirectory)))
}

// WriteModuleState stores the value of a script's write_state in the
// check's state directory, and ReadModuleState returns it, or nil when the
// script has stored none.
func WriteModuleState(stateDirectory string, value any) error {
	written, e := PlainJSON(value)
	if e != nil {
		return e
	}
	if len(written) > ModuleStateLimit {
		return errors.New("the state is more than 1 MiB as JSON")
	}
	// The value arrives whole or not at all: a run that ends in the middle
	// of the write leaves the earlier value.
	path := filepath.Join(stateDirectory, ModuleStateFile)
	if e := os.WriteFile(path+".new", written, 0600); e != nil {
		return e
	}
	return os.Rename(path+".new", path)
}

func ReadModuleState(stateDirectory string) (any, error) {
	written, e := primitives.ReadFile(filepath.Join(stateDirectory, ModuleStateFile), ModuleStateLimit)
	if errors.Is(e, fs.ErrNotExist) {
		return nil, nil
	}
	if e != nil {
		return nil, e
	}
	return decodeJSONValue(written)
}
