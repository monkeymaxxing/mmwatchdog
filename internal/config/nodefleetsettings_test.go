package config_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mmwatchdog/internal/config"
	"mmwatchdog/internal/testhelpers"
)

// Silence alert destinations are optional. Local destinations must exist;
// joined nodes can only validate the syntax of the hub's destination names.
func TestSilenceAlertDestinationsAreValidated(t *testing.T) {
	t.Parallel()
	const field = "send_silence_alerts_of_this_node_only_to_destinations"
	for _, role := range []string{"", "fleet_hub_role: {report_every: 30s, alert_after_hub_unreachable_for: 3m, send_alert_without_hub_after_waiting_for: 30s, undelivered_alerts_of_nodes_content_size_limit: 32MiB, listen_on_address: '0.0.0.0:8445', address_for_nodes: 'hub.test:8445'}\n", "join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code}\n"} {
		for list, problem := range map[string]string{
			"[]": "", "[owner]": "", "[missing]": "alert_destinations.yml has no destination named", "[bad name]": "not a valid destination name", "[owner, owner]": "twice", "owner": "expected a list",
		} {
			joined := strings.Contains(role, "join_fleet_hub")
			if joined && list == "[missing]" {
				problem = ""
			}
			path := testhelpers.ConfigFile(t, role+field+": "+list+"\ncheck_groups: {app: {shell: 'true'}}\n")
			if !joined {
				if err := os.WriteFile(filepath.Join(path, "alert_destinations.yml"), []byte("destinations: {owner: {webhook: {url: 'https://example.com/owner'}, remind_every: never, alert_severities: []}}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := config.Load(path)
			if problem == "" && err != nil || problem != "" && (err == nil || !strings.Contains(err.Error(), problem)) {
				t.Errorf("role %q list %s: got %v, want %q", role, list, err, problem)
			}
		}
	}
}

// raise_to_sos_when_every_node_of_these_node_groups_is_silent is optional,
// and names only node groups that the node states in
// this_node_is_in_node_groups, by the same rules for names and duplicates.
// The hub checks what a node sends in the same way.
func TestRaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilentIsValidated(t *testing.T) {
	t.Parallel()
	const field = "raise_to_sos_when_every_node_of_these_node_groups_is_silent"
	checks := "check_groups: {app: {shell: 'true'}}\n"
	for text, want := range map[string]string{
		"this_node_is_in_node_groups: [jobs_servers]\n" + field + ": [frankfurt]\n": field + `: names the node group "frankfurt", which the node is not in: add the node group to this_node_is_in_node_groups, or remove the node group from raise_to_sos_when_every_node_of_these_node_groups_is_silent`,
		field + ": [jobs_servers]\n": field + `: names the node group "jobs_servers", which the node is not in`,
		"this_node_is_in_node_groups: [jobs_servers]\n" + field + ": [jobs servers]\n":               field + `: "jobs servers" is not a valid name of a node group`,
		"this_node_is_in_node_groups: [jobs_servers]\n" + field + ": [jobs_servers, jobs_servers]\n": field + `: names the node group "jobs_servers" twice`,
		"this_node_is_in_node_groups: [jobs_servers]\n" + field + ": jobs_servers\n":                 field + ": expected a list",
	} {
		if _, err := config.Load(testhelpers.ConfigFile(t, text+checks)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: got %v, want %q", text, err, want)
		}
	}
	for text, want := range map[string][]string{
		"this_node_is_in_node_groups: [jobs_servers]\n":                                           nil,
		"this_node_is_in_node_groups: [jobs_servers]\n" + field + ": []\n":                        {},
		"this_node_is_in_node_groups: [frankfurt, jobs_servers]\n" + field + ": [jobs_servers]\n": {"jobs_servers"},
		"join_fleet_hub: {join_code_file: /etc/mmwatchdog/secrets/fleet-hub-join-code}\nthis_node_is_in_node_groups: [frankfurt, jobs_servers]\n" + field + ": [jobs_servers, frankfurt]\n": {"jobs_servers", "frankfurt"},
	} {
		c, err := config.Load(testhelpers.ConfigFile(t, text+checks))
		if err != nil {
			t.Errorf("%q: %v", text, err)
			continue
		}
		if got := c.NodeFleetSettings(); got.Problem() != "" || !slices.Equal(got.RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent, want) {
			t.Errorf("%q: %+v", text, got)
		}
	}
	// What another machine sends is checked in the same way.
	for settings, want := range map[*config.NodeFleetSettings]string{
		{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"jobs_servers"}, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"frankfurt"}}:                    `has an invalid ` + field + `: names the node group "frankfurt", which the node is not in`,
		{AlertSeverityWhenThisNodeGoesSilent: "warn", RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"jobs_servers"}}:                                                                   `has an invalid ` + field + `: names the node group "jobs_servers", which the node is not in`,
		{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"jobs_servers"}, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"jobs_servers", "jobs_servers"}}: `has an invalid ` + field + `: names the node group "jobs_servers" twice`,
		{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"jobs_servers"}, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"a b"}}:                          `has an invalid ` + field + `: "a b" is not a valid name of a node group`,
		{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"jobs_servers"}, RaiseToSosWhenEveryNodeOfTheseNodeGroupsIsSilent: []string{"jobs_servers"}}:                 "",
		{AlertSeverityWhenThisNodeGoesSilent: "warn", ThisNodeIsInNodeGroups: []string{"jobs_servers"}}:                                                                                             "",
	} {
		if got := settings.Problem(); want == "" && got != "" || want != "" && !strings.Contains(got, want) {
			t.Errorf("%+v: got %q, want %q", *settings, got, want)
		}
	}
}
