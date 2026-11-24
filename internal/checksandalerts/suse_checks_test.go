package checksandalerts_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// The tests of the SUSE part of the checks of security updates, a required
// reboot, and the end of security support: zypper_security_patches, and the
// SUSE parts of reboot_after_updates and
// distribution_release_security_support. zypper is a stand-in on PATH.

const leapRelease = "ID=\"opensuse-leap\"\nID_LIKE=\"suse opensuse\"\nPRETTY_NAME=\"openSUSE Leap 16.1\"\n"

// zypperRepositoryFile is the file of a repository in /etc/zypp/repos.d.
func zypperRepositoryFile(alias, enabled, service string) string {
	text := "[" + alias + "]\nname=" + alias + "\nenabled=" + enabled + "\nautorefresh=1\n"
	if service != "" {
		text += "service=" + service + "\n"
	}
	return text
}

// patchesXML is what zypper --xmlout list-patches writes for the patches.
func patchesXML(patches ...string) string {
	return "<?xml version='1.0'?>\n<stream>\n<message type=\"info\">Loading repository data...</message>\n<update-status version=\"0.6\">\n<update-list>\n" +
		strings.Join(patches, "") + "</update-list>\n</update-status>\n</stream>\n"
}

func patchXML(name, kind, category, severity, restart, status, summary string) string {
	return "<update name=\"" + name + "\" edition=\"1\" kind=\"" + kind + "\" category=\"" + category + "\" severity=\"" + severity +
		"\" restart=\"" + restart + "\" status=\"" + status + "\">\n<summary>" + summary + "</summary>\n<description>Long text.</description>\n</update>\n"
}

func TestZypperSecurityPatchesCheck(t *testing.T) {
	ranAs := asThisAccount(t)
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.ZypperSecurityPatches{FailWhen: "a_security_patch_waits_to_be_installed"}})
	}
	// Four enabled repositories, two of them of a service, and a disabled
	// one that has no directory in the cache.
	root := nodeFiles(t, map[string]string{
		"etc/os-release":                                               leapRelease,
		"etc/zypp/repos.d/repo-oss.repo":                               zypperRepositoryFile("openSUSE:repo-oss", "1", "openSUSE"),
		"etc/zypp/repos.d/repo-non.repo":                               zypperRepositoryFile("openSUSE:repo-non-oss", "1", "openSUSE"),
		"etc/zypp/repos.d/own.repo":                                    zypperRepositoryFile("own", "1", ""),
		"etc/zypp/repos.d/unlisted.repo":                               "[no-enabled-key]\nname=no enabled key\n",
		"etc/zypp/repos.d/disabled.repo":                               zypperRepositoryFile("openSUSE:repo-debug", "0", "openSUSE"),
		"var/cache/zypp/raw/openSUSE:repo-oss/repodata/repomd.xml":     "",
		"var/cache/zypp/raw/openSUSE:repo-non-oss/repodata/repomd.xml": "",
		"var/cache/zypp/raw/own/repodata/repomd.xml":                   "",
		"var/cache/zypp/raw/no-enabled-key/repodata/repomd.xml":        "",
	})
	answer, exitCode, asked := clientProgram(t, "zypper", "LC_ALL")
	set := func(content string) { os.WriteFile(answer, []byte(content), 0644) }
	const reason = ", so MMWatchdog cannot know which patches wait: set up a daily refresh, a systemd timer that runs zypper --non-interactive refresh, as the page of zypper_security_patches shows (mmwatchdog describe-module zypper_security_patches)"

	// No security patch waits. Other patches and other kinds do not count.
	set(patchesXML(
		patchXML("openSUSE-Leap-16.1-20", "patch", "recommended", "moderate", "false", "needed", "Recommended update for tzdata"),
		patchXML("openSUSE-Leap-16.1-21", "patch", "security", "important", "false", "not-needed", "Security update for the installed"),
		patchXML("some-package", "package", "security", "important", "false", "needed", "Not a patch")))
	if o := observe(); o.Status != "ok" || o.Message != "no security patch waits to be installed" {
		t.Fatal(o)
	}
	// zypper runs as the unprivileged account, without a refresh.
	if data, _ := os.ReadFile(asked); string(data) != "--non-interactive\n--no-refresh\n--xmlout\nlist-patches\n--category\nsecurity\nLC_ALL=C\n" || !reflect.DeepEqual(ranAs(), []string{"mmwatchdog"}) {
		t.Fatalf("%q", data)
	}
	// One patch, and then several in the order of their names. A patch that
	// needs a reboot says so. A patch that two repositories list counts once.
	set(patchesXML(patchXML("openSUSE-Leap-16.1-12", "patch", "security", "important", "false", "needed", "Security update for libXtst")))
	if o := observe(); o.Status != "failed" || o.Message != "1 security patch waits to be installed: openSUSE-Leap-16.1-12 (important): Security update for libXtst" {
		t.Fatal(o)
	}
	set(patchesXML(
		patchXML("openSUSE-Leap-16.1-30", "patch", "security", "critical", "true", "needed", "Security update for the Linux kernel"),
		patchXML("openSUSE-Leap-16.1-12", "patch", "security", "important", "false", "needed", "Security update for libXtst"),
		patchXML("openSUSE-Leap-16.1-12", "patch", "security", "important", "false", "needed", "Security update for libXtst")))
	o := observe()
	if o.Status != "failed" || o.Message != "2 security patches wait to be installed: openSUSE-Leap-16.1-12 (important): Security update for libXtst, openSUSE-Leap-16.1-30 (critical): Security update for the Linux kernel (needs a reboot)" {
		t.Fatal(o)
	}
	// Each patch is one finding, with an incident of its own. Its exact
	// finding is the release, the patch, its severity, and whether it needs
	// a reboot.
	wantFindings := []checksandalerts.Finding{
		{ID: "openSUSE-Leap-16.1-12", Message: "the security patch openSUSE-Leap-16.1-12 (important): Security update for libXtst waits to be installed", ExactFinding: "opensuse-leap openSUSE-Leap-16.1-12 important; no reboot needed"},
		{ID: "openSUSE-Leap-16.1-30", Message: "the security patch openSUSE-Leap-16.1-30 (critical): Security update for the Linux kernel (needs a reboot) waits to be installed", ExactFinding: "opensuse-leap openSUSE-Leap-16.1-30 critical; reboot needed"},
	}
	if !reflect.DeepEqual(o.Findings, wantFindings) || !(&checksandalerts.Check{Inputs: new(checksandalerts.ZypperSecurityPatches)}).Module().OneIncidentForEachFinding ||
		o.FindingIsGoneMessage("openSUSE-Leap-16.1-12") != "the security patch openSUSE-Leap-16.1-12 no longer waits to be installed" {
		t.Fatalf("%+v", o.Findings)
	}

	// Exit code 106 means that zypper skipped a repository. Its error text
	// makes the check unknown, also when the XML lists a patch.
	os.WriteFile(exitCode, []byte("106"), 0644)
	set("<?xml version='1.0'?>\n<stream>\n<message type=\"error\">Download (curl) error for 'https://example.invalid/repo/repodata/repomd.xml': Could not resolve host</message>\n</stream>\n")
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot ask zypper which patches wait: Download (curl) error for 'https://example.invalid/repo/repodata/repomd.xml': Could not resolve host" {
		t.Fatal(o)
	}
	// Another code is an error too, with the error of the standard error
	// when the XML has none.
	os.WriteFile(exitCode, []byte("7"), 0644)
	set("")
	os.WriteFile(answer+".stderr", []byte("System management is locked by the application with pid 1234.\n"), 0644)
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot ask zypper which patches wait: System management is locked by the application with pid 1234." {
		t.Fatal(o)
	}
	os.Remove(answer + ".stderr")
	// An error message with exit code 0 counts as well.
	os.WriteFile(exitCode, []byte("0"), 0644)
	set(patchesXML() + "<message type=\"error\">Repository 'x' is invalid.</message>\n")
	if o := observe(); o.Status != "unknown" || !strings.Contains(o.Message, "Repository 'x' is invalid.") {
		t.Fatal(o)
	}
	set("this is not XML <")
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "MMWatchdog cannot read what zypper wrote: ") {
		t.Fatal(o)
	}
	set(patchesXML())

	// The metadata of every enabled repository must be there and at most
	// seven days old; the oldest decides.
	old := time.Now().Add(-9 * 24 * time.Hour)
	older := time.Now().Add(-12 * 24 * time.Hour)
	recent := time.Now().Add(-6 * 24 * time.Hour)
	repomd := func(directory string) string {
		return filepath.Join(root, "var/cache/zypp/raw", directory, "repodata/repomd.xml")
	}
	os.Chtimes(repomd("own"), recent, recent)
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	os.Chtimes(repomd("openSUSE:repo-non-oss"), old, old)
	os.Chtimes(repomd("openSUSE:repo-oss"), older, older)
	// The alias of a repository of a service carries the service, and is its directory.
	if o := observe(); o.Status != "unknown" || o.Message != "/var/cache/zypp/raw/openSUSE:repo-oss/repodata/repomd.xml, which zypper writes after a successful refresh of the repository openSUSE:repo-oss, is 12 days old"+reason {
		t.Fatal(o)
	}
	os.Chtimes(repomd("openSUSE:repo-oss"), recent, recent)
	if o := observe(); o.Status != "unknown" || o.Message != "/var/cache/zypp/raw/openSUSE:repo-non-oss/repodata/repomd.xml, which zypper writes after a successful refresh of the repository openSUSE:repo-non-oss, is 9 days old"+reason {
		t.Fatal(o)
	}
	os.Chtimes(repomd("openSUSE:repo-non-oss"), recent, recent)
	// A repository without its file is unknown, and a disabled repository
	// needs none.
	os.Remove(repomd("own"))
	if o := observe(); o.Status != "unknown" || o.Message != "the repository own has no usable /var/cache/zypp/raw/own/repodata/repomd.xml, which zypper writes after a successful refresh of the repository"+reason {
		t.Fatal(o)
	}
	os.WriteFile(repomd("own"), nil, 0644)
	// A repository that does not say enabled= is enabled.
	os.Remove(repomd("no-enabled-key"))
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "the repository no-enabled-key has no usable ") {
		t.Fatal(o)
	}
	os.WriteFile(repomd("no-enabled-key"), nil, 0644)
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	// No repository at all.
	os.RemoveAll(filepath.Join(root, "etc/zypp/repos.d"))
	if o := observe(); o.Status != "unknown" || o.Message != "zypper has no enabled repository in /etc/zypp/repos.d, so MMWatchdog cannot know which patches wait" {
		t.Fatal(o)
	}

	// A node of another family uses another check.
	nodeFiles(t, map[string]string{"etc/os-release": "ID=debian\nPRETTY_NAME=\"Debian GNU/Linux 13 (trixie)\"\n"})
	if o := observe(); o.Status != "unknown" || o.Message != "this node runs Debian GNU/Linux 13 (trixie), whose package manager is apt, not zypper: use the check apt_security_updates" {
		t.Fatal(o)
	}
	aptSecurityUpdates := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.AptSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}})
	}
	nodeFiles(t, map[string]string{"etc/os-release": leapRelease})
	if o := aptSecurityUpdates(); o.Status != "unknown" || o.Message != "this node runs openSUSE Leap 16.1, whose package manager is zypper, not apt: use the check zypper_security_patches" {
		t.Fatal(o)
	}

	loadChecks(t, map[string]string{
		"{zypper_security_patches: {fail_when: a_security_patch_waits_to_be_installed}, alert_severity: warn}": "",
		"{zypper_security_patches: {fail_when: another_rule}, alert_severity: warn}":                           "check_groups.c.zypper_security_patches.fail_when: expected one of a_security_patch_waits_to_be_installed",
		"{zypper_security_patches: {}, alert_severity: warn}":                                                  "check_groups.c.zypper_security_patches.fail_when: is required",
	})
}

// On SUSE, zypper creates /run/reboot-needed, without package names.
func TestRebootAfterUpdatesCheckOnSUSE(t *testing.T) {
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.RebootAfterUpdates{FailWhen: "the_node_must_reboot_to_use_installed_updates"}})
	}
	root := nodeFiles(t, map[string]string{"etc/os-release": leapRelease, "run/": ""})
	// No hook program needs to exist: zypper itself writes the file.
	if o := observe(); o.Status != "ok" || o.Message != "this node needs no reboot" {
		t.Fatal(o)
	}
	os.WriteFile(filepath.Join(root, "run/reboot-needed"), nil, 0644)
	if o := observe(); o.Status != "failed" || o.Message != "this node needs a reboot (/run/reboot-needed exists)" {
		t.Fatal(o)
	}
	// The Debian file does not count on SUSE.
	os.Remove(filepath.Join(root, "run/reboot-needed"))
	os.WriteFile(filepath.Join(root, "run/reboot-required"), nil, 0644)
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
}

// On SUSE, the end of the release is the endoflife of the product file.
func TestDistributionReleaseSecuritySupportCheckOnSUSE(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(301 * 24 * time.Hour).Format("2006-01-02")
	soon := now.Add(101 * 24 * time.Hour).Format("2006-01-02")
	ended := now.Add(-40 * 24 * time.Hour).Format("2006-01-02")
	product := func(releaseEnd, lineEnd string) string {
		text := "<product schemeversion=\"0\">\n <vendor>openSUSE</vendor>\n <name>Leap</name>\n <version>16.1</version>\n"
		if releaseEnd != "" {
			text += " <endoflife>" + releaseEnd + "</endoflife>\n"
		}
		text += " <codestream>\n  <name>Leap 16</name>\n"
		if lineEnd != "" {
			text += "  <endoflife>" + lineEnd + "</endoflife>\n"
		}
		return text + " </codestream>\n</product>\n"
	}
	for _, tc := range []struct{ name, support, duration, file, status, message string }{
		{"healthy", "regular", "180d", product(future, soon), "ok", "this node runs openSUSE Leap 16.1: SUSE's regular security support ends on " + future + ", in 9 months"},
		{"the release's end wins over the end of the line", "regular", "180d", product(soon, future), "failed", "this node runs openSUSE Leap 16.1: SUSE's regular security support ends on " + soon + ", in 3 months"},
		{"the end of the line does not count", "regular", "180d", product("", soon), "unknown", "/etc/products.d/baseproduct gives no endoflife for this release"},
		{"ended", "regular", "180d", product(ended, future), "failed", "this node runs openSUSE Leap 16.1: SUSE's regular security support ended on " + ended + ", 40 days ago"},
		{"no endoflife", "regular", "180d", product("", ""), "unknown", "/etc/products.d/baseproduct gives no endoflife for this release"},
		{"unreadable date", "regular", "180d", product("soon", ""), "unknown", "MMWatchdog cannot read the endoflife of /etc/products.d/baseproduct: parsing time \"soon\" as \"2006-01-02\": cannot parse \"soon\" as \"2006\""},
		{"no file", "regular", "180d", "", "unknown", "MMWatchdog cannot read /etc/products.d/baseproduct, the product file of this release, which the release package of openSUSE Leap 16.1 installs"},
		{"not XML", "regular", "180d", "<product", "unknown", ""},
		{"Debian LTS", "debian_lts", "180d", product(future, ""), "unknown", "security_support: debian_lts applies only to Debian, and this node runs openSUSE Leap 16.1"},
		{"Ubuntu Pro", "ubuntu_pro", "180d", product(future, ""), "unknown", "security_support: ubuntu_pro applies only to Ubuntu, and this node runs openSUSE Leap 16.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The node has no VERSION_CODENAME.
			files := map[string]string{"etc/os-release": leapRelease}
			if tc.file != "" {
				files["etc/products.d/baseproduct"] = tc.file
			}
			nodeFiles(t, files)
			o := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.DistributionReleaseSecuritySupport{SecuritySupport: tc.support, FailWhenSecuritySupportEndsWithin: checksandalerts.Duration(tc.duration)}})
			if o.Status != tc.status || tc.message != "" && o.Message != tc.message || tc.message == "" && !strings.HasPrefix(o.Message, "MMWatchdog cannot read /etc/products.d/baseproduct: ") {
				t.Fatalf("got %v, want %s: %s", o, tc.status, tc.message)
			}
		})
	}
}
