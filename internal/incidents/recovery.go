package incidents

import (
	"slices"
	"time"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/config"
)

// Policy governs one recovery budget: a systemd unit's restarts, shared by
// every check group whose restart names that unit, or one group's recovery command. Policies and
// budgets are keyed by unit or by check group; a unit name ends in .service and a
// check group name has no dot, so the two never collide.
type Policy struct {
	CheckGroups []string
	When        []string // check keys whose health governs the budget
	// After is how long each check in when must be failing before the
	// action is due: the restart_after_failing_for of the check group whose
	// restart lists it, or the command's run_after_failing_for.
	After map[string]time.Duration
	// Attempts is the number of attempts, or NoAttemptLimit.
	Attempts                      int
	MinTimeBetweenAttempts, Reset time.Duration
	// Run is the check group's recovery command, or nil for a unit's restarts.
	Run *checksandalerts.RecoveryCommand
}

// RecoveryPolicies computes the restart policy of each unit, whose check
// groups state identical settings for its shared budget, and the policy of each
// check group's recovery command. An unchanged policy keeps its previous value,
// which serves as the ticket for actions already in progress.
func RecoveryPolicies(c *config.Config, previous map[string]*Policy) map[string]*Policy {
	policies := map[string]*Policy{}
	for _, name := range checksandalerts.Names(c.CheckGroups) {
		ch := c.CheckGroups[name]
		if r := ch.RecoveryCommand; r != nil {
			attempts, _ := r.Attempts.Limit()
			p := &Policy{CheckGroups: []string{name}, After: map[string]time.Duration{}, Attempts: attempts, MinTimeBetweenAttempts: r.MinTimeBetweenAttempts.Value(), Reset: r.ResetAttemptsAfterHealthyFor.Value(), Run: r}
			for _, check := range r.TriggeredByChecks {
				p.When = append(p.When, name+"/"+check)
				p.After[name+"/"+check] = r.RunAfterFailingFor.Value()
			}
			policies[name] = p
		}
		r := ch.RestartSystemdUnit
		if r == nil {
			continue
		}
		// Validation requires identical budget settings for this unit.
		attempts, _ := r.Attempts.Limit()
		p := policies[r.SystemdUnit]
		if p == nil {
			p = &Policy{After: map[string]time.Duration{}, Attempts: attempts, MinTimeBetweenAttempts: r.MinTimeBetweenAttempts.Value(), Reset: r.ResetAttemptsAfterHealthyFor.Value()}
			policies[r.SystemdUnit] = p
		}
		p.CheckGroups = append(p.CheckGroups, name)
		for _, check := range r.TriggeredByChecks {
			key := name + "/" + check
			if !slices.Contains(p.When, key) {
				p.When = append(p.When, key)
			}
			p.After[key] = r.RestartAfterFailingFor.Value()
		}
	}
	for key, p := range policies {
		slices.Sort(p.When)
		if old := previous[key]; old != nil && config.Fingerprint(policyValue(old)) == config.Fingerprint(policyValue(p)) {
			policies[key] = old
		}
	}
	return policies
}

func policyValue(p *Policy) []any {
	return []any{p.CheckGroups, p.When, p.After, p.Attempts, p.MinTimeBetweenAttempts, p.Reset, p.Run}
}

func MaxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
