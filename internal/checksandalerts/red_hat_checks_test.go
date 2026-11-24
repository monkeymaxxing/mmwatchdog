package checksandalerts_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// The tests of the Red Hat family's part of the checks of security updates,
// a required reboot, and the end of security support: dnf_security_updates,
// and the Red Hat parts of reboot_after_updates and
// distribution_release_security_support. dnf is a stand-in on PATH.

const rockyRelease = "ID=\"rocky\"\nID_LIKE=\"rhel centos fedora\"\nPRETTY_NAME=\"Rocky Linux 9.8 (Blue Onyx)\"\n"

// dnfRepositoryFile is the file of a repository in /etc/yum.repos.d.
func dnfRepositoryFile(id, enabled string) string {
	text := "[" + id + "]\nname=" + id + "\n"
	if enabled != "" {
		text += "enabled=" + enabled + "\n"
	}
	return text + "gpgcheck=1\n"
}

func TestDnfSecurityUpdatesCheck(t *testing.T) {
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.DnfSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}})
	}
	// Three enabled repositories, one of them without enabled=, and a
	// disabled one. The cache has the directory of each, and the directory
	// of an older configuration of baseos, which is older than the current
	// one.
	root := nodeFiles(t, map[string]string{
		"etc/os-release":             rockyRelease,
		"etc/yum.repos.d/rocky.repo": dnfRepositoryFile("baseos", "1") + dnfRepositoryFile("baseos-debuginfo", "0") + dnfRepositoryFile("appstream", "1"),
		"etc/yum.repos.d/own.repo":   dnfRepositoryFile("own", ""),
		"var/cache/dnf/baseos-0123456789abcdef/repodata/aaaa-primary.xml.gz":           "data",
		"var/cache/dnf/baseos-fedcba9876543210/repodata/bbbb-primary.xml.gz":           "data",
		"var/cache/dnf/baseos-debuginfo-0123456789abcdef/repodata/cccc-primary.xml.gz": "data",
		"var/cache/dnf/appstream-0123456789abcdef/repodata/dddd-primary.xml.gz":        "data",
		"var/cache/dnf/own-0123456789abcdef/repodata/eeee-primary.xml.gz":              "data",
		"var/cache/dnf/last_makecache":                                                 "",
	})
	answer, exitCode, asked := clientProgram(t, "dnf", "LC_ALL")
	set := func(content string) { os.WriteFile(answer, []byte(content), 0644) }
	const reason = ", so MMWatchdog cannot know which updates wait: enable the refresh timer of dnf, systemctl enable --now dnf-makecache.timer, as the page of dnf_security_updates shows (mmwatchdog describe-module dnf_security_updates)"
	primary := func(directory, file string) string {
		return filepath.Join(root, "var/cache/dnf", directory, "repodata", file)
	}

	// No security update waits.
	set("")
	if o := observe(); o.Status != "ok" || o.Message != "no security update waits to be installed" {
		t.Fatal(o)
	}
	// dnf is asked only for the data that it holds, in its own words.
	if data, _ := os.ReadFile(asked); string(data) != "--quiet\n--cacheonly\nupdateinfo\nlist\n--security\n--updates\nLC_ALL=C\n" {
		t.Fatalf("%q", data)
	}
	// A package that several advisories name counts once, with the name
	// cut from its epoch, version, release, and architecture.
	set("RLSA-2026:1001 Important/Sec. bind-libs-32:9.16.23-18.el9_4.1.x86_64\n" +
		"RLSA-2026:1002 Moderate/Sec. bind-libs-32:9.16.23-19.el9_4.1.x86_64\n" +
		"RLSA-2026:2002 Critical/Sec. kernel-5.14.0-503.el9.x86_64\n")
	o := observe()
	if o.Status != "failed" || o.Message != "2 security updates wait to be installed: bind-libs, kernel" {
		t.Fatal(o)
	}
	expectPackageSecurity(t, o, new(checksandalerts.DnfSecurityUpdates), "rocky", "no security update waits to be installed")
	set("RLSA-2026:2002 Critical/Sec. kernel-5.14.0-503.el9.x86_64\n")
	if o := observe(); o.Status != "failed" || o.Message != "1 security update waits to be installed: kernel" {
		t.Fatal(o)
	}
	// A kernel that is a year old has 27 advisories: still one finding.
	yearOld := ""
	for advisory := range 27 {
		yearOld += fmt.Sprintf("RLSA-2025:%04d Important/Sec. kernel-5.14.0-%d.el9.x86_64\n", 1000+advisory, 400+advisory)
	}
	set(yearOld)
	if o := observe(); o.Message != "1 security update waits to be installed: kernel" || len(o.Findings) != 1 {
		t.Fatalf("%+v", o)
	}
	// A list in another form is not understood.
	set("Last metadata expiration check: 0:10:00 ago\n")
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "dnf gave a list of security updates that MMWatchdog does not understand: ") {
		t.Fatal(o)
	}
	set("")
	// An error beside exit code 0, and a failed dnf.
	os.WriteFile(answer+".stderr", []byte("Error: Cache-only enabled but no cache for 'baseos'\n"), 0644)
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot ask dnf which updates wait: Error: Cache-only enabled but no cache for 'baseos'" {
		t.Fatal(o)
	}
	os.WriteFile(exitCode, []byte("1"), 0644)
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot ask dnf which updates wait: Error: Cache-only enabled but no cache for 'baseos'" {
		t.Fatal(o)
	}
	os.Remove(answer + ".stderr")
	os.WriteFile(exitCode, []byte("0"), 0644)

	// The package data of every enabled repository must be there and at most
	// seven days old; the oldest decides. Of the directories of one
	// repository, the newest file counts. The stamp last_makecache counts
	// for nothing.
	old := time.Now().Add(-9 * 24 * time.Hour)
	older := time.Now().Add(-12 * 24 * time.Hour)
	recent := time.Now().Add(-6 * 24 * time.Hour)
	os.Chtimes(primary("baseos-0123456789abcdef", "aaaa-primary.xml.gz"), older, older)
	os.Chtimes(primary("baseos-fedcba9876543210", "bbbb-primary.xml.gz"), recent, recent)
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	// Only the directory of the repository's own id counts: baseos-debuginfo is
	// another repository, and a disabled one.
	os.Chtimes(primary("baseos-debuginfo-0123456789abcdef", "cccc-primary.xml.gz"), older, older)
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	os.Chtimes(primary("baseos-fedcba9876543210", "bbbb-primary.xml.gz"), old, old)
	os.Chtimes(primary("appstream-0123456789abcdef", "dddd-primary.xml.gz"), older, older)
	if o := observe(); o.Status != "unknown" || o.Message != "the package data of the repository appstream (a file *primary.* in /var/cache/dnf/appstream-0123456789abcdef/repodata), which dnf touches at each successful refresh of the repository, is 12 days old"+reason {
		t.Fatal(o)
	}
	os.Chtimes(primary("appstream-0123456789abcdef", "dddd-primary.xml.gz"), recent, recent)
	if o := observe(); o.Status != "unknown" || o.Message != "the package data of the repository baseos (a file *primary.* in /var/cache/dnf/baseos-fedcba9876543210/repodata), which dnf touches at each successful refresh of the repository, is 9 days old"+reason {
		t.Fatal(o)
	}
	os.Chtimes(primary("baseos-fedcba9876543210", "bbbb-primary.xml.gz"), recent, recent)
	// A fresh stamp does not help a repository whose package data is old.
	os.Chtimes(primary("own-0123456789abcdef", "eeee-primary.xml.gz"), old, old)
	os.Chtimes(filepath.Join(root, "var/cache/dnf/last_makecache"), time.Now(), time.Now())
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "the package data of the repository own ") {
		t.Fatal(o)
	}
	os.Chtimes(primary("own-0123456789abcdef", "eeee-primary.xml.gz"), recent, recent)
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	// A repository without package data, or with an empty file, is unknown.
	os.WriteFile(primary("appstream-0123456789abcdef", "dddd-primary.xml.gz"), nil, 0644)
	if o := observe(); o.Status != "unknown" || o.Message != "the repository appstream has no package data in dnf's cache (a file *primary.* in /var/cache/dnf/appstream-HASH/repodata)"+reason {
		t.Fatal(o)
	}
	os.WriteFile(primary("appstream-0123456789abcdef", "dddd-primary.xml.gz"), []byte("data"), 0644)
	// No repository at all.
	os.RemoveAll(filepath.Join(root, "etc/yum.repos.d"))
	if o := observe(); o.Status != "unknown" || o.Message != "dnf has no enabled repository in /etc/yum.repos.d, so MMWatchdog cannot know which updates wait" {
		t.Fatal(o)
	}

	// A node of another family uses another check, and each of the other
	// checks names this one.
	nodeFiles(t, map[string]string{"etc/os-release": "ID=debian\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n"})
	if o := observe(); o.Status != "unknown" || o.Message != "this node runs Debian GNU/Linux 13 (trixie), whose package manager is apt, not dnf: use the check apt_security_updates" {
		t.Fatal(o)
	}
	nodeFiles(t, map[string]string{"etc/os-release": leapRelease})
	if o := observe(); o.Status != "unknown" || o.Message != "this node runs openSUSE Leap 16.1, whose package manager is zypper, not dnf: use the check zypper_security_patches" {
		t.Fatal(o)
	}
	nodeFiles(t, map[string]string{"etc/os-release": rockyRelease})
	apt := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.AptSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}})
	if apt.Status != "unknown" || apt.Message != "this node runs Rocky Linux 9.8 (Blue Onyx), whose package manager is dnf, not apt: use the check dnf_security_updates" {
		t.Fatal(apt)
	}
	zypper := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.ZypperSecurityPatches{FailWhen: "a_security_patch_waits_to_be_installed"}})
	if zypper.Status != "unknown" || zypper.Message != "this node runs Rocky Linux 9.8 (Blue Onyx), whose package manager is dnf, not zypper: use the check dnf_security_updates" {
		t.Fatal(zypper)
	}

	loadChecks(t, map[string]string{
		"{dnf_security_updates: {fail_when: a_security_update_waits_to_be_installed}, alert_severity: warn}": "",
		"{dnf_security_updates: {fail_when: another_rule}, alert_severity: warn}":                            "check_groups.c.dnf_security_updates.fail_when: expected one of a_security_update_waits_to_be_installed",
		"{dnf_security_updates: {}, alert_severity: warn}":                                                   "check_groups.c.dnf_security_updates.fail_when: is required",
	})
}

// On the Red Hat family, dnf's needs-restarting command says whether the node
// needs a reboot, through its exit code and its standard output.
func TestRebootAfterUpdatesCheckOnTheRedHatFamily(t *testing.T) {
	ranAs := asThisAccount(t)
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.RebootAfterUpdates{FailWhen: "the_node_must_reboot_to_use_installed_updates"}})
	}
	nodeFiles(t, map[string]string{"etc/os-release": rockyRelease})
	// Without dnf on PATH, the check is unknown.
	t.Setenv("PATH", t.TempDir())
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot find the program dnf") {
		t.Fatal(o)
	}
	ranAs() // The account that the first run looked up.
	answer, exitCode, asked := clientProgram(t, "dnf", "LC_ALL")
	set := func(code, output, errors string) {
		os.WriteFile(exitCode, []byte(code), 0644)
		os.WriteFile(answer, []byte(output), 0644)
		os.WriteFile(answer+".stderr", []byte(errors), 0644)
	}
	set("0", "No core libraries or services have been updated since boot-up.\nReboot should not be necessary.\n", "")
	if o := observe(); o.Status != "ok" || o.Message != "this node needs no reboot" {
		t.Fatal(o)
	}
	// dnf runs as the unprivileged account, without any repository.
	data, _ := os.ReadFile(asked)
	if accounts := ranAs(); string(data) != "--quiet\n--disablerepo=*\nneeds-restarting\n--reboothint\nLC_ALL=C\n" || !reflect.DeepEqual(accounts, []string{"mmwatchdog"}) {
		t.Fatalf("%q %v", data, accounts)
	}
	// Exit code 1 with reasons: the node needs a reboot, and the reasons are the details.
	set("1", "Core libraries or services have been updated since boot-up:\n  * glibc\n\nReboot is required to fully utilize these updates.\n", "")
	o := observe()
	if o.Status != "failed" || o.Message != "this node needs a reboot (dnf needs-restarting --reboothint)" ||
		!reflect.DeepEqual(o.Details, []string{"Core libraries or services have been updated since boot-up:", "* glibc", "Reboot is required to fully utilize these updates."}) {
		t.Fatal(o)
	}
	// The plugin is not installed.
	set("1", "", "No such command: needs-restarting. Please use /usr/bin/dnf --help\nIt could be a DNF plugin command, try: \"dnf install 'dnf-command(needs-restarting)'\"\n")
	if o := observe(); o.Status != "unknown" || o.Message != "the dnf of this node has no command needs-restarting, which says whether the node needs a reboot: install the package of dnf's plugins, with dnf install dnf-plugins-core" {
		t.Fatal(o)
	}
	// Exit code 1 without reasons is a program that failed.
	set("1", "", "Error: something else\n")
	if o := observe(); o.Status != "unknown" || o.Message != "dnf needs-restarting ended with exit code 1 and no reason for a reboot: Error: something else" {
		t.Fatal(o)
	}
}

// On the Red Hat family, the end of the release is the SUPPORT_END of
// /etc/os-release.
func TestDistributionReleaseSecuritySupportCheckOnTheRedHatFamily(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(301 * 24 * time.Hour).Format("2006-01-02")
	soon := now.Add(101 * 24 * time.Hour).Format("2006-01-02")
	ended := now.Add(-40 * 24 * time.Hour).Format("2006-01-02")
	release := func(supportEnd string) string {
		text := rockyRelease
		if supportEnd != "" {
			text += "SUPPORT_END=" + supportEnd + "\n"
		}
		return text
	}
	for _, tc := range []struct{ name, support, file, status, message string }{
		{"healthy", "regular", release(future), "ok", "this node runs Rocky Linux 9.8 (Blue Onyx): the regular security support that the release states ends on " + future + ", in 9 months"},
		// Rocky Linux quotes the date; AlmaLinux does not.
		{"quoted date", "regular", release("\"" + soon + "\""), "failed", "this node runs Rocky Linux 9.8 (Blue Onyx): the regular security support that the release states ends on " + soon + ", in 3 months"},
		{"ended", "regular", release(ended), "failed", "this node runs Rocky Linux 9.8 (Blue Onyx): the regular security support that the release states ended on " + ended + ", 40 days ago"},
		{"no SUPPORT_END", "regular", release(""), "unknown", "/etc/os-release gives no SUPPORT_END for this release, which the release package of Rocky Linux 9.8 (Blue Onyx) installs"},
		{"unreadable date", "regular", release("soon"), "unknown", "MMWatchdog cannot read the SUPPORT_END of /etc/os-release: parsing time \"soon\" as \"2006-01-02\": cannot parse \"soon\" as \"2006\""},
		{"Debian LTS", "debian_lts", release(future), "unknown", "security_support: debian_lts applies only to Debian, and this node runs Rocky Linux 9.8 (Blue Onyx)"},
		{"Ubuntu Pro", "ubuntu_pro", release(future), "unknown", "security_support: ubuntu_pro applies only to Ubuntu, and this node runs Rocky Linux 9.8 (Blue Onyx)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeFiles(t, map[string]string{"etc/os-release": tc.file})
			o := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.DistributionReleaseSecuritySupport{SecuritySupport: tc.support, FailWhenSecuritySupportEndsWithin: "180d"}})
			if o.Status != tc.status || o.Message != tc.message {
				t.Fatalf("got %v, want %s: %s", o, tc.status, tc.message)
			}
		})
	}
}
