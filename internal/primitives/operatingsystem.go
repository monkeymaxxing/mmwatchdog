package primitives

import (
	"slices"
	"strings"
)

// NodeOSRelease reads the fields of /etc/os-release, with quotes trimmed.
// An unreadable file has no fields.
func NodeOSRelease() map[string]string {
	fields := map[string]string{}
	data, err := ReadFile(NodeFile("/etc/os-release"), ProcFileLimit)
	if err != nil {
		return fields
	}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, found := strings.Cut(line, "=")
		if found {
			fields[key] = strings.Trim(strings.TrimSpace(value), `"'`)
		}
	}
	return fields
}

// DistributionFamily is the group of distributions that share a package
// manager and the files that it leaves. The empty family is a distribution
// that MMWatchdog does not know.
type DistributionFamily string

const (
	DebianFamily DistributionFamily = "debian"
	SUSEFamily   DistributionFamily = "suse"
	RedHatFamily DistributionFamily = "redhat"
)

// distributionFamilies says, for each family, which name in the ID or the
// ID_LIKE of /etc/os-release belongs to it. A new family is one more line.
var distributionFamilies = []struct {
	family DistributionFamily
	has    func(name string) bool
	// ownIDs are IDs that belong to the family only as the ID of the
	// distribution itself, never as a name in its ID_LIKE.
	ownIDs []string
}{
	{DebianFamily, func(name string) bool { return name == "debian" || name == "ubuntu" }, nil},
	// Leap has ID opensuse-leap and ID_LIKE "suse opensuse"; SUSE Linux
	// Enterprise has ID sles and ID_LIKE suse.
	{SUSEFamily, func(name string) bool { return strings.Contains(name, "suse") }, nil},
	// Red Hat Enterprise Linux, and the distributions like it: Rocky Linux and
	// AlmaLinux have their own ID and say "rhel" in ID_LIKE, and so does
	// CentOS Stream. Fedora has ID fedora and no ID_LIKE, so it joins by its
	// own ID. Amazon Linux says only that it is like fedora, and is no member:
	// a name in ID_LIKE never counts for ownIDs. The checks of the family are
	// unknown there, and never misread the output of dnf.
	{RedHatFamily, func(name string) bool { return slices.Contains([]string{"rhel", "rocky", "almalinux", "centos"}, name) }, []string{"fedora"}},
}

// NodeDistributionFamily reads /etc/os-release and finds the node's family
// by ID or ID_LIKE. distributionName is PRETTY_NAME, for messages.
func NodeDistributionFamily() (family DistributionFamily, distributionName string) {
	fields := NodeOSRelease()
	names := append([]string{fields["ID"]}, strings.Fields(fields["ID_LIKE"])...)
	for _, candidate := range distributionFamilies {
		if slices.Contains(candidate.ownIDs, fields["ID"]) {
			return candidate.family, fields["PRETTY_NAME"]
		}
		for _, name := range names {
			if candidate.has(name) {
				return candidate.family, fields["PRETTY_NAME"]
			}
		}
	}
	return "", fields["PRETTY_NAME"]
}
