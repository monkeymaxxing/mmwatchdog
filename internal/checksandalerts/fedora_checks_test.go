package checksandalerts_test

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/primitives"
	"mmwatchdog/internal/testhelpers"
)

// The tests of Fedora's part of the Red Hat family: dnf 5, which is a
// program dnf5 with the link dnf, answers in JSON. dnf5 is a stand-in on PATH.

const fedoraRelease = "ID=fedora\nVERSION_ID=45\nPRETTY_NAME=\"Fedora Linux 45\"\nSUPPORT_END=2027-12-08\n"

// dnfFiveRepository is one entry of "dnf repo info --enabled --json".
func dnfFiveRepository(id string, age, expire time.Duration) map[string]any {
	return map[string]any{"id": id, "is_enabled": true, "timestamp": time.Now().Add(-age).Unix(), "metadata_expire": int64(expire.Seconds())}
}

func jsonText(t *testing.T, value any) string {
	t.Helper()
	text, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

// dnfFiveProgram makes a node whose dnf is a link to dnf5. The stand-in
// answers "repo info" from the file repo_info and "updateinfo" from the file
// listing, each with its exit code and standard error, and notes the
// arguments of every call in the file asked.
func dnfFiveProgram(t *testing.T) (say func(file, text string), asked func() string) {
	t.Helper()
	files := t.TempDir()
	say = func(file, text string) {
		if err := os.WriteFile(filepath.Join(files, file), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, command := range []string{"repo_info", "listing"} {
		say(command, "")
		say(command+".exit_code", "0")
		say(command+".standard_error", "")
	}
	quoted := testhelpers.ShellQuote(files)
	script := "printf '%s\\n' \"$*\" \"LC_ALL=$LC_ALL\" >> " + quoted + "/asked\n" +
		"case \" $* \" in\n" +
		"*' repo info '*) which=repo_info;;\n" +
		"*' updateinfo '*) which=listing;;\n" +
		"*) echo \"unexpected call: $*\" >&2; exit 99;;\n" +
		"esac\n" +
		"cat " + quoted + "/$which; cat " + quoted + "/$which.standard_error >&2; exit $(cat " + quoted + "/$which.exit_code)"
	programs := t.TempDir()
	testhelpers.WriteStandInProgram(t, filepath.Join(programs, "dnf5"), "#!/bin/sh\n"+script+"\n")
	testhelpers.WriteStandInProgram(t, filepath.Join(programs, "dnf"), "#!/bin/sh\n"+script+"\n")
	if err := os.Remove(filepath.Join(programs, "dnf")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("dnf5", filepath.Join(programs, "dnf")); err != nil {
		t.Fatal(err)
	}
	testhelpers.PutProgramsFirst(t, programs)
	return say, func() string {
		data, _ := os.ReadFile(filepath.Join(files, "asked"))
		return string(data)
	}
}

func TestDnfSecurityUpdatesCheckOnDnfFive(t *testing.T) {
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.DnfSecurityUpdates{FailWhen: "a_security_update_waits_to_be_installed"}})
	}
	nodeFiles(t, map[string]string{"etc/os-release": fedoraRelease})
	say, asked := dnfFiveProgram(t)
	const hour, day = time.Hour, 24 * time.Hour
	const reason = ", so MMWatchdog cannot know which updates wait: enable the refresh timer of dnf, systemctl enable --now dnf-makecache.timer, as the page of dnf_security_updates shows (mmwatchdog describe-module dnf_security_updates)"
	// Fedora's three repositories, each refreshed lately for its own
	// metadata_expire: the updates every 6 hours, the release every 7 days,
	// and the openh264 repository every 14 days.
	repositories := func(updates, release, openh264 time.Duration) string {
		return jsonText(t, []map[string]any{
			dnfFiveRepository("fedora", release, 7*day),
			dnfFiveRepository("fedora-cisco-openh264", openh264, 14*day),
			dnfFiveRepository("updates", updates, 6*hour),
		})
	}
	say("repo_info", repositories(2*hour, 3*day, 5*day))

	// No security update waits: dnf writes an empty JSON array. dnf is asked
	// for its repositories and then for the list, in its own words, from its
	// cache, in JSON.
	say("listing", "[]\n")
	if o := observe(); o.Status != "ok" || o.Message != "no security update waits to be installed" {
		t.Fatal(o)
	}
	if got := asked(); got != "--quiet --cacheonly repo info --enabled --json\nLC_ALL=C\n--quiet --cacheonly updateinfo list --security --updates --json\nLC_ALL=C\n" {
		t.Fatalf("%q", got)
	}

	// A package that several advisories name counts once, with the name cut
	// from its version, release, and architecture.
	say("listing", `[
  {"name":"FEDORA-2026-3dc7e56d1e","type":"security","severity":"Important","nevra":"pcre2-10.49-1.fc45.x86_64","buildtime":1794157739},
  {"name":"FEDORA-2026-3dc7e56d1e","type":"security","severity":"Important","nevra":"pcre2-syntax-10.49-1.fc45.noarch","buildtime":1794157739},
  {"name":"FEDORA-2026-aaaaaaaaaa","type":"security","severity":"Moderate","nevra":"pcre2-10.48-1.fc45.x86_64","buildtime":1794157700}
]`)
	o := observe()
	if o.Status != "failed" || o.Message != "2 security updates wait to be installed: pcre2, pcre2-syntax" {
		t.Fatal(o)
	}
	expectPackageSecurity(t, o, new(checksandalerts.DnfSecurityUpdates), "fedora 45", "no security update waits to be installed")

	// Every list that is not understood is unknown.
	for name, written := range map[string]string{
		"dnf 5's text list":       "Name Type Severity Package Issued\nFEDORA-2026-3dc7e56d1e security Important pcre2-10.49-1.fc45.x86_64 2026-11-08 17:08:59\n",
		"no output":               "",
		"null":                    "null",
		"an object":               `{"type":"security","nevra":"pcre2-10.49-1.fc45.x86_64"}`,
		"another type":            `[{"type":"bugfix","nevra":"pcre2-10.49-1.fc45.x86_64"}]`,
		"no type":                 `[{"nevra":"pcre2-10.49-1.fc45.x86_64"}]`,
		"no package":              `[{"type":"security"}]`,
		"an empty package":        `[{"type":"security","nevra":""}]`,
		"a package of a number":   `[{"type":"security","nevra":4}]`,
		"text after the list":     "[]\nLast metadata expiration check",
		"a list cut off":          `[{"type":"security","nevra":"pcre2-10.49-1.fc45.x86_6`,
		"one good entry, one bad": `[{"type":"security","nevra":"pcre2-10.49-1.fc45.x86_64"},{"type":"enhancement","nevra":"vim-9-1.fc45.x86_64"}]`,
	} {
		say("listing", written)
		if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "dnf gave a list of security updates that MMWatchdog does not understand:") {
			t.Errorf("%s: %v", name, o)
		}
	}
	say("listing", "[]")

	// A failed dnf is unknown, with dnf's words.
	say("listing.exit_code", "1")
	say("listing.standard_error", "Cache-only enabled but no cache for repository \"updates\"\n")
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot ask dnf which updates wait: Cache-only enabled but no cache for repository \"updates\"" {
		t.Fatal(o)
	}
	say("listing.exit_code", "0")
	say("listing.standard_error", "")

	// The repository data of dnf 5 may be as old as the repository's own
	// metadata_expire and one day more, and at least 7 days.
	say("repo_info", repositories(6*day, 8*day-hour, 15*day-hour))
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	say("repo_info", repositories(8*day, 3*day, 5*day))
	if o := observe(); o.Status != "unknown" || o.Message != "the package data of the repository updates, which dnf touches at each successful refresh of the repository, is 8 days old (the limit for this repository is 7 days)"+reason {
		t.Fatal(o)
	}
	say("repo_info", repositories(2*hour, 8*day+hour, 5*day))
	if o := observe(); o.Status != "unknown" || o.Message != "the package data of the repository fedora, which dnf touches at each successful refresh of the repository, is 8 days old (the limit for this repository is 8 days)"+reason {
		t.Fatal(o)
	}
	say("repo_info", repositories(2*hour, 3*day, 15*day+hour))
	if o := observe(); o.Status != "unknown" || o.Message != "the package data of the repository fedora-cisco-openh264, which dnf touches at each successful refresh of the repository, is 15 days old (the limit for this repository is 15 days)"+reason {
		t.Fatal(o)
	}
	// A metadata_expire of months raises the limit only to 15 days, and the
	// message names the repository's own setting as the cause.
	say("repo_info", jsonText(t, []map[string]any{dnfFiveRepository("own", 16*day, 90*day)}))
	if o := observe(); o.Status != "unknown" || o.Message != "the package data of the repository own, which dnf touches at each successful refresh of the repository, is 16 days old (the limit for this repository is 15 days), so MMWatchdog cannot know which updates wait: the repository's metadata_expire of 90 days is longer than MMWatchdog accepts for security data; set it to at most 14 days in the repository's file" {
		t.Fatal(o)
	}
	say("repo_info", jsonText(t, []map[string]any{dnfFiveRepository("own", 14*day, 90*day)}))
	if o := observe(); o.Status != "ok" {
		t.Fatal(o)
	}
	// The oldest repository decides.
	say("repo_info", repositories(10*day, 3*day, 30*day))
	if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "the package data of the repository fedora-cisco-openh264, ") {
		t.Fatal(o)
	}
	// A repository without data has the timestamp 0. A metadata_expire that
	// is not a time (dnf's never) leaves the limit at 7 days.
	say("repo_info", jsonText(t, []map[string]any{dnfFiveRepository("updates", hour, 6*hour), {"id": "own", "timestamp": 0, "metadata_expire": 3600}}))
	if o := observe(); o.Status != "unknown" || o.Message != "the repository own has no package data in dnf's cache"+reason {
		t.Fatal(o)
	}
	say("repo_info", jsonText(t, []map[string]any{{"id": "own", "timestamp": time.Now().Add(-8 * day).Unix(), "metadata_expire": -1}}))
	if o := observe(); o.Status != "unknown" || !strings.Contains(o.Message, "is 8 days old (the limit for this repository is 7 days)") {
		t.Fatal(o)
	}

	// Without an enabled repository, or with a list of repositories that is
	// not understood, or with a dnf that fails, the check is unknown.
	say("repo_info", "[]")
	if o := observe(); o.Status != "unknown" || o.Message != "dnf has no enabled repository, so MMWatchdog cannot know which updates wait" {
		t.Fatal(o)
	}
	for name, written := range map[string]string{
		"no output":      "",
		"null":           "null",
		"an object":      `{"id":"updates"}`,
		"no timestamp":   `[{"id":"updates","metadata_expire":21600}]`,
		"no expiry":      fmt.Sprintf(`[{"id":"updates","timestamp":%d}]`, time.Now().Unix()),
		"no id":          fmt.Sprintf(`[{"timestamp":%d,"metadata_expire":21600}]`, time.Now().Unix()),
		"a text time":    `[{"id":"updates","timestamp":"yesterday","metadata_expire":21600}]`,
		"dnf 4's output": "Repo ID : updates\n",
	} {
		say("repo_info", written)
		if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "dnf gave a list of repositories that MMWatchdog does not understand:") {
			t.Errorf("%s: %v", name, o)
		}
	}
	say("repo_info", repositories(hour, hour, hour))
	say("repo_info.exit_code", "1")
	say("repo_info.standard_error", "Cache-only enabled but no cache for repository \"updates\"\n")
	if o := observe(); o.Status != "unknown" || o.Message != "MMWatchdog cannot ask dnf about its repositories: Cache-only enabled but no cache for repository \"updates\"" {
		t.Fatal(o)
	}
}

// Amazon Linux says only that it is like fedora, and dnf 5 on Fedora does
// not make it a member of the Red Hat family.
func TestFedoraIsInTheRedHatFamilyByItsOwnID(t *testing.T) {
	for release, want := range map[string]primitives.DistributionFamily{
		fedoraRelease: primitives.RedHatFamily,
		"ID=\"amzn\"\nID_LIKE=\"fedora\"\nPRETTY_NAME=\"Amazon Linux 2023\"\n":    "",
		"ID=bazzite\nID_LIKE=\"fedora\"\nPRETTY_NAME=\"Bazzite\"\n":               "",
		"ID=\"nobara\"\nID_LIKE=\"rhel centos fedora\"\nPRETTY_NAME=\"Nobara\"\n": primitives.RedHatFamily,
	} {
		nodeFiles(t, map[string]string{"etc/os-release": release})
		if family, _ := primitives.NodeDistributionFamily(); family != want {
			t.Errorf("%q: %q", release, family)
		}
	}
}

// On Fedora, dnf 5's needs-restarting command says whether the node needs a
// reboot, through its exit code and its JSON.
func TestRebootAfterUpdatesCheckOnFedora(t *testing.T) {
	ranAs := asThisAccount(t)
	observe := func() checksandalerts.Observation {
		return withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.RebootAfterUpdates{FailWhen: "the_node_must_reboot_to_use_installed_updates"}})
	}
	nodeFiles(t, map[string]string{"etc/os-release": fedoraRelease})
	answer, exitCode, asked := clientProgramWithLink(t, "dnf5", "dnf", "LC_ALL")
	set := func(code, output, errors string) {
		os.WriteFile(exitCode, []byte(code), 0644)
		os.WriteFile(answer, []byte(output), 0644)
		os.WriteFile(answer+".stderr", []byte(errors), 0644)
	}
	const notNeeded = `[{"type":"reboot","reboot_required":false,"packages":[],"documentation":"https://access.redhat.com/solutions/27943"}]`
	const needed = `[{"type":"reboot","reboot_required":true,"packages":["kernel","glibc","glibc"],"documentation":"https://access.redhat.com/solutions/27943"}]`
	set("0", notNeeded, "")
	if o := observe(); o.Status != "ok" || o.Message != "this node needs no reboot" {
		t.Fatal(o)
	}
	// dnf runs as the unprivileged account, without any repository, and
	// without the option --reboothint, which dnf 5 ignores.
	data, _ := os.ReadFile(asked)
	if accounts := ranAs(); string(data) != "--quiet\n--disablerepo=*\nneeds-restarting\n--json\nLC_ALL=C\n" || !reflect.DeepEqual(accounts, []string{"mmwatchdog"}) {
		t.Fatalf("%q %v", data, accounts)
	}
	// Exit code 1 and the reasons: the node needs a reboot.
	set("1", needed, "")
	o := observe()
	if o.Status != "failed" || o.Message != "this node needs a reboot (dnf needs-restarting)" ||
		!reflect.DeepEqual(o.Details, []string{"Core libraries or services have been updated since boot-up: glibc, kernel"}) {
		t.Fatal(o)
	}
	// The plugin is not installed.
	set("2", "", "Unknown argument \"needs-restarting\" for command \"dnf5\". Add \"--help\" for more information about the arguments.\nIt could be a command provided by a plugin, try: dnf5 install 'dnf5-command(needs-restarting)'\n")
	if o := observe(); o.Status != "unknown" || o.Message != "the dnf of this node has no command needs-restarting, which says whether the node needs a reboot: install the package of dnf's plugins, with dnf install dnf5-plugins" {
		t.Fatal(o)
	}
	// An exit code and an answer that disagree, an answer of another form,
	// and a program that failed are unknown.
	for name, tc := range map[string]struct{ code, output, errors string }{
		"exit 0, reboot needed":     {"0", needed, ""},
		"exit 1, no reboot needed":  {"1", notNeeded, ""},
		"exit 2, no reboot needed":  {"2", notNeeded, ""},
		"exit 1, no answer":         {"1", "", "Error: something else\n"},
		"dnf 4's text":              {"1", "Core libraries or services have been updated since boot-up:\n  * glibc\n", ""},
		"an object":                 {"0", `{"type":"reboot","reboot_required":false}`, ""},
		"two entries":               {"0", `[{"type":"reboot","reboot_required":false},{"type":"reboot","reboot_required":false}]`, ""},
		"another type":              {"0", `[{"type":"services","reboot_required":false}]`, ""},
		"no reboot_required":        {"0", `[{"type":"reboot","packages":[]}]`, ""},
		"reboot_required of a text": {"1", `[{"type":"reboot","reboot_required":"yes"}]`, ""},
	} {
		set(tc.code, tc.output, tc.errors)
		if o := observe(); o.Status != "unknown" || !strings.HasPrefix(o.Message, "dnf needs-restarting ended with exit code "+tc.code+" and an answer that MMWatchdog does not understand:") {
			t.Errorf("%s: %v", name, o)
		}
	}
}

// Fedora states the end of its release in SUPPORT_END, as the rest of the
// Red Hat family does.
func TestDistributionReleaseSecuritySupportCheckOnFedora(t *testing.T) {
	end := time.Now().UTC().Add(101 * 24 * time.Hour).Format("2006-01-02")
	nodeFiles(t, map[string]string{"etc/os-release": strings.Replace(fedoraRelease, "2027-12-08", end, 1)})
	o := withinTimeLimit(t, 5*time.Second, checksandalerts.Check{Inputs: &checksandalerts.DistributionReleaseSecuritySupport{SecuritySupport: "regular", FailWhenSecuritySupportEndsWithin: "180d"}})
	if o.Status != "failed" || o.Message != "this node runs Fedora Linux 45: the regular security support that the release states ends on "+end+", in 3 months" {
		t.Fatal(o)
	}
}
