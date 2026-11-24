package config_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

const (
	hubRole    = "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445'}\n"
	joinedRole = "join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code}\n"
	someChecks = "check_groups: {app: {shell: 'true'}}\n"
)

// The entry of the design of setting a finding aside, as the configuration
// files reference shows it: the date unquoted, as YAML reads a timestamp.
const openSSHEntry = `  - check: installed_packages/known_exploited_flaws
    finding: CVE-2026-123158 openssh
    exact_finding: "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z"
    reason: Needs musl without PAM; every server with this exact finding runs glibc and sshd links PAM. Debian rates it unimportant.
    investigation_note: triage_agent_notes/2026-11-23_openssh_package_warning_on_app001.md
    set_aside_by: triage agent (MMHotline run b82fdd8b)
    set_aside_on: 2026-11-23
`

func configWithSetAsideFindings(t *testing.T, role, text string) string {
	t.Helper()
	directory := testhelpers.ConfigFile(t, role+someChecks)
	if err := os.WriteFile(filepath.Join(directory, config.SetAsideFindingsFile), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return directory
}

// A hub reads its set-aside entries; an empty list sets nothing aside, and
// a hub without the file has none.
func TestAHubReadsItsSetAsideFindings(t *testing.T) {
	t.Parallel()
	c, err := config.Load(configWithSetAsideFindings(t, hubRole, "set_aside_findings:\n"+openSSHEntry))
	if err != nil {
		t.Fatal(err)
	}
	want := config.SetAsideFinding{Check: "installed_packages/known_exploited_flaws", Finding: "CVE-2026-123158 openssh",
		ExactFinding:      "Debian:13 CVE-2026-123158 openssh 1:10.0p1-7+deb13u4; fixed version: none; DEBIAN-CVE-2026-123158 modified 2026-11-20T08:12:00Z",
		Reason:            "Needs musl without PAM; every server with this exact finding runs glibc and sshd links PAM. Debian rates it unimportant.",
		InvestigationNote: "triage_agent_notes/2026-11-23_openssh_package_warning_on_app001.md", SetAsideBy: "triage agent (MMHotline run b82fdd8b)", SetAsideOn: "2026-11-23"}
	if len(c.SetAsideFindings) != 1 || c.SetAsideFindings[0] != want {
		t.Fatalf("got %+v", c.SetAsideFindings)
	}
	for _, text := range []string{"set_aside_findings: []\n", "set_aside_findings: []\n# set aside nothing\n"} {
		if c, err := config.Load(configWithSetAsideFindings(t, hubRole, text)); err != nil || len(c.SetAsideFindings) != 0 {
			t.Fatalf("%q: %v %v", text, c, err)
		}
	}
	if c, err := config.Load(testhelpers.ConfigFile(t, hubRole+someChecks)); err != nil || c.SetAsideFindings != nil {
		t.Fatal("a hub without the file", err)
	}
}

// Only a hub may have the file: a joined node and a standalone node refuse
// it, because nodes never set anything aside.
func TestOnlyAHubMayHaveSetAsideFindings(t *testing.T) {
	t.Parallel()
	for _, role := range []string{"", joinedRole} {
		_, err := config.Load(configWithSetAsideFindings(t, role, "set_aside_findings: []\n"))
		if err == nil || !strings.Contains(err.Error(), "set_aside_findings.yml: only a fleet hub sets findings aside, and this node has no fleet_hub_role setting; remove the file from this node") {
			t.Errorf("role %q: got %v", role, err)
		}
	}
}

// Every field of an entry is required and not empty; the check and the
// finding have their forms; two entries for one exact finding are one too
// many; and a file without the list says how to write an empty one.
func TestEveryFieldOfASetAsideEntryIsRequired(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"":                                     `expected a set_aside_findings: list; write "set_aside_findings: []" to set no finding aside`,
		"set_aside_findings:\n":                "set_aside_findings: expected a list, found nothing",
		"set_aside_findings: {}\n":             "set_aside_findings: expected a list",
		"set_aside_findings: [{check: a/b}]\n": "set_aside_findings.0.finding: is required and must not be empty",
		"set_aside_findings:\n" + openSSHEntry + "  - nickname: x\n":                                                                                  `set_aside_findings.1.nickname: unknown field "nickname"`,
		"set_aside_findings:\n" + strings.Replace(openSSHEntry, "check: installed_packages/known_exploited_flaws", "check: known_exploited_flaws", 1): "set_aside_findings.0.check: expected CHECK_GROUP/CHECK",
		"set_aside_findings:\n" + strings.Replace(openSSHEntry, "set_aside_on: 2026-11-23", "set_aside_on: last tuesday", 1):                          "set_aside_findings.0.set_aside_on: expected a date such as 2026-11-23",
		"set_aside_findings:\n" + strings.Replace(openSSHEntry, "set_aside_on: 2026-11-23", `set_aside_on: "2026-11-23"`, 1) + openSSHEntry:           "set_aside_findings.1: sets aside the same check, finding, and exact finding as set_aside_findings.0",
	}
	for _, field := range []string{"finding", "exact_finding", "reason", "investigation_note", "set_aside_by"} {
		lines := strings.Split(openSSHEntry, "\n")
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimLeft(line, " -"), field+":") {
				prefix := line[:strings.Index(line, field)]
				lines[i] = prefix + field + `: " "`
			}
		}
		cases["set_aside_findings:\n"+strings.Join(lines, "\n")] = "set_aside_findings.0." + field + ": is required and must not be empty"
		missing := []string{}
		for _, line := range strings.Split(openSSHEntry, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), field+":") {
				continue
			}
			missing = append(missing, line)
		}
		cases["set_aside_findings:\n"+strings.Join(missing, "\n")] = "set_aside_findings.0." + field + ": is required and must not be empty"
	}
	for text, want := range cases {
		if _, err := config.Load(configWithSetAsideFindings(t, hubRole, text)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
}

// An entry covers only an exact match of the check, the finding, and the
// exact finding, and never an incident at sos.
func TestASetAsideEntryCoversOnlyAnExactMatchBelowSos(t *testing.T) {
	t.Parallel()
	entries := config.SetAsideFindings{{Check: "packages/flaws", Finding: "CVE-1 openssh", ExactFinding: "openssh 1.0; fixed version: none"}}
	if _, covered := entries.Covering("packages/flaws", "CVE-1 openssh", "openssh 1.0; fixed version: none", "warn"); !covered {
		t.Fatal("an exact match below sos is not covered")
	}
	for _, c := range [][4]string{
		{"packages/flaws", "CVE-1 openssh", "openssh 1.0; fixed version: none", "sos"},
		{"packages/flaws", "CVE-1 openssh", "openssh 1.1; fixed version: none", "warn"},
		{"packages/flaws", "CVE-1 openssh", "openssh 1.0; fixed version: 1.2", "info"},
		{"packages/other", "CVE-1 openssh", "openssh 1.0; fixed version: none", "warn"},
		{"packages/flaws", "CVE-2 openssh", "openssh 1.0; fixed version: none", "warn"},
		{"packages/flaws", "CVE-1 openssh", "", "warn"},
	} {
		if _, covered := entries.Covering(c[0], c[1], c[2], c[3]); covered {
			t.Errorf("%q is covered", c)
		}
	}
}
