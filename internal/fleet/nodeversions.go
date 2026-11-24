package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"mmwatchdog/internal/store"
)

// JoinedNodeVersions reads, from the hub's fleet.db in stateDir, the
// MMWatchdog version that each joined node's latest report states, by node
// name. It only reads, so mmwatchdog validate can ask while the hub runs.
// A node without a report is left out, and so is everything when the hub
// has no fleet.db yet or it cannot be read: the versions only feed a
// validation warning.
func JoinedNodeVersions(stateDir string) map[string]string {
	versions := map[string]string{}
	path := filepath.Join(stateDir, "fleet.db")
	if _, e := os.Stat(path); e != nil {
		return versions
	}
	db, e := store.OpenReadOnlyDatabase(path)
	if e != nil {
		return versions
	}
	defer db.Close()
	rows, e := db.Query("SELECT name, data FROM nodes")
	if e != nil {
		return versions
	}
	defer func() {
		_ = rows.Close() // This cursor only reads data; scan and iteration errors already determine the result.
	}()
	for rows.Next() {
		var name string
		var data []byte
		var m member
		if rows.Scan(&name, &data) != nil || json.Unmarshal(data, &m) != nil {
			continue
		}
		// The hub's own entry has no credential.
		if m.Credential == "" || m.NodeStatusReport == nil {
			continue
		}
		versions[name] = m.NodeStatusReport.Build.Version
	}
	return versions
}

// ReleaseOlderThan reports that version names a release, such as 0.1.3,
// older than the release major.minor. A development build, such as
// 0.1.0-dev, names no release, so it is never older.
func ReleaseOlderThan(version string, major, minor int) bool {
	parts := strings.Split(strings.TrimPrefix(version, "v"), ".")
	if len(parts) != 3 {
		return false
	}
	numbers := [3]int{}
	for i, part := range parts {
		n, e := strconv.Atoi(part)
		if e != nil || n < 0 || strconv.Itoa(n) != part {
			return false
		}
		numbers[i] = n
	}
	return numbers[0] < major || numbers[0] == major && numbers[1] < minor
}
