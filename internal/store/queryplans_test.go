package store

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"mmwatchdog/internal/config"
)

// The hub's statements that run for each report, each alert, each delivery
// step, or each pruning read their rows through an index, so their cost
// follows the rows they need, not the size of the fleet's tables: the
// design of grouping by exact finding, "Each arrival is constant work".
func TestTheHubsFrequentStatementsUseAnIndex(t *testing.T) {
	t.Parallel()
	s, e := OpenStore(filepath.Join(t.TempDir(), "state"), config.DefaultStorageLimits)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	scan := regexp.MustCompile(`\bSCAN (fleet_incident_set|fleet_direct_sends_settled|fleet_node_incidents|fleet_incident_keys|intake)\b`)
	for name, statement := range map[string]string{
		"settled sends of a reporter":      settledSendsOfReporter,
		"failures waiting in the intake":   failuresWaitingInTheIntake,
		"alerts waiting in the intake":     alertsWaitingInTheIntake,
		"pending changes of a key":         pendingChangesOfKey,
		"prune resolved set rows":          pruneResolvedSetRows,
		"prune settled direct sends":       pruneSettledDirectSends,
		"prune handled alerts":             pruneHandledAlerts,
		"due updates and reminders, first": dueKeys("next_update_due"),
		"due updates and reminders, then":  dueKeys("next_reminder_due"),
	} {
		args := make([]any, strings.Count(statement, "?"))
		rows, e := s.DB.Query("EXPLAIN QUERY PLAN "+statement, args...)
		if e != nil {
			t.Fatal(name, e)
		}
		plan := []string{}
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if e = rows.Scan(&id, &parent, &unused, &detail); e != nil {
				t.Fatal(e)
			}
			plan = append(plan, detail)
		}
		_ = rows.Close()
		if scan.MatchString(strings.Join(plan, "\n")) {
			t.Errorf("%s scans a table: %q", name, plan)
		}
		// A key of 5000 nodes with one pending change reads one row.
		if statement == pendingChangesOfKey && !strings.Contains(strings.Join(plan, "\n"), "fleet_incident_set_pending_changes") {
			t.Errorf("%s reads every row of its key: %q", name, plan)
		}
	}
}
