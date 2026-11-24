package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode"

	"mmwatchdog/internal/checksandalerts"
	"mmwatchdog/internal/fleet"
)

// printFleet compresses healthy nodes to a count and expands the rest. It
// exits with the worst node's code. The view of one node group shows the
// nodes of that node group and leaves out what is about the whole fleet: the
// hub-outage alerter nodes and the refused joins.
func printFleet(b []byte, asJSON bool, now time.Time) error {
	var f fleet.Fleet
	if e := json.Unmarshal(b, &f); e != nil {
		return e
	}
	if asJSON {
		if e := printJSON(f); e != nil {
			return e
		}
	} else {
		f = cleaned(f)
		healthy := 0
		for _, s := range f.Nodes {
			if isHealthy(s, now) {
				healthy++
			}
		}
		if f.NodeGroup != "" {
			fmt.Printf("Node group %s of the fleet at %s: %s, %d healthy\n", f.NodeGroup, f.Address, countOf(len(f.Nodes), "node"), healthy)
		} else {
			fmt.Printf("Fleet at %s: %s, %d healthy\n", f.Address, countOf(len(f.Nodes), "node"), healthy)
		}
		for _, s := range f.Nodes {
			if !isHealthy(s, now) {
				printNodeLine(s, now)
			}
		}
		if f.SetAsideFindingCount > 0 {
			fmt.Printf("Set aside: %s on %s\n", countOf(f.SetAsideFindingCount, "finding"), countOf(f.NodesWithSetAsideFindings, "node"))
		}
		if f.NodeGroup == "" {
			outageAlerters := []string{}
			for _, s := range f.Nodes {
				if s.OutageAlerter {
					outageAlerters = append(outageAlerters, s.Name+outageAlerterPreference(s))
				}
			}
			if len(outageAlerters) == 0 {
				outageAlerters = append(outageAlerters, "none; no other node reports")
			}
			fmt.Println("Hub-outage alerter nodes:", strings.Join(outageAlerters, ", "))
		}
		for _, s := range f.Nodes {
			if s.NodeStatusReport != nil {
				for _, n := range checksandalerts.Names(s.NodeStatusReport.Maintenance) {
					fmt.Printf("Maintenance: %s %s %s\n", s.Name, n, s.NodeStatusReport.Maintenance[n].Describe())
				}
			}
		}
		// How far back the hub's measurement history of nodes reaches is a
		// fact about the whole fleet too, and no storage problem.
		if sentence := f.MeasurementHistoryOfNodes.CutShortSentence(now); sentence != "" && f.NodeGroup == "" {
			fmt.Println(sentence)
		}
		if r := f.RefusedJoins; r.Count > 0 && f.NodeGroup == "" {
			fmt.Printf("Refused joins: %d with a wrong join code, latest from %s at %s\n", r.Count, r.Address, r.Last.Format(time.RFC3339))
		}
		if f.StorageError != "" {
			fmt.Println("Storage:", f.StorageError)
		}
	}
	if f.ExitCode != 0 {
		return exitStatus(f.ExitCode)
	}
	return nil
}

func outageAlerterPreference(s fleet.FleetNode) string {
	if s.PreferOutageAlerter {
		return " (prefer_this_node_as_hub_outage_alerter is enabled)"
	}
	return ""
}

// isHealthy reports a node that needs no attention: reporting, healthy by
// its own summary, with a sound clock and no recent claim on its name.
func isHealthy(s fleet.FleetNode, now time.Time) bool {
	return s.State == "reporting" && s.ExitCode == 0 && s.ClockOffset == 0 && !recentConflict(s, now) && (s.NodeStatusReport == nil || len(s.NodeStatusReport.Delivery.FailingDestinations) == 0)
}
func recentConflict(s fleet.FleetNode, now time.Time) bool {
	return s.Conflict != nil && now.Sub(s.Conflict.At) < time.Hour
}

// printNodeLine prints an expanded node and what needs attention on it. A
// silent node's details are its last known state.
func printNodeLine(s fleet.FleetNode, now time.Time) {
	line := fmt.Sprintf("%-24s %-10s exit %d", s.Name, s.State, s.ExitCode)
	if s.NodeStatusReport != nil && s.State != "silent" {
		line += "  " + nodeResourceLine(s.NodeStatusReport.NodeResources)
	}
	fmt.Println(line + "  " + contact(s, now))
	if len(s.NodeGroups) > 0 {
		fmt.Println("  Node groups:", strings.Join(s.NodeGroups, ", "))
	}
	if s.ClockOffset != 0 {
		fmt.Printf("  Clock offset: %s\n", s.ClockOffset.Round(time.Second))
	}
	if recentConflict(s, now) {
		fmt.Printf("  Name conflict: a node at %s claimed this name at %s\n", s.Conflict.Address, s.Conflict.At.Format(time.RFC3339))
	}
	if s.NodeStatusReport == nil {
		return
	}
	prefix := "  "
	if s.State == "silent" {
		prefix = "  last known: "
	}
	for _, a := range s.NodeStatusReport.Summary.Attention {
		// An entry about one failed systemd unit names the unit after its check.
		about := a.Check
		if a.Finding != "" {
			about += ": " + a.Finding
		}
		// A check that has not run yet has no time to show.
		since := ""
		if !a.Since.IsZero() {
			since = " since " + a.Since.Format(time.RFC3339)
		}
		fmt.Printf("%s%s %s%s: %s\n", prefix, about, a.State, since, a.Message)
	}
	for _, failing := range s.NodeStatusReport.Delivery.FailingDestinations {
		fmt.Println(prefix + failing.Describe())
	}
	if s.NodeStatusReport.StorageError != "" {
		fmt.Printf("%sstorage: %s\n", prefix, s.NodeStatusReport.StorageError)
	}
}

// contact describes the last report, or the join when there is none.
func contact(s fleet.FleetNode, now time.Time) string {
	if s.LastReport.IsZero() {
		return "no report since joining at " + s.Joined.Format(time.RFC3339)
	}
	return fmt.Sprintf("last report %s (%s ago)", s.LastReport.Format(time.RFC3339), now.Sub(s.LastReport).Round(time.Second))
}

func nodeResourceLine(node map[string]checksandalerts.Measurement) string {
	parts := []string{}
	for _, n := range []string{"cpu", "memory", "disk"} {
		if m, ok := node[n]; ok {
			parts = append(parts, fmt.Sprintf("%s %.0f%%", n, m.Value))
		}
	}
	return strings.Join(parts, "  ")
}

// printNode shows one node's checks, incidents with evidence, recent
// events, and last contact. It exits with that node's code.
func printNode(b []byte, asJSON bool, now time.Time) error {
	var s fleet.FleetNode
	if e := json.Unmarshal(b, &s); e != nil {
		return e
	}
	if asJSON {
		if e := printJSON(s); e != nil {
			return e
		}
	} else {
		s = cleaned(s)
		fmt.Printf("%s: %s, exit %d\n", s.Name, s.State, s.ExitCode)
		if s.StorageError != "" {
			fmt.Println("Storage:", s.StorageError)
		}
		fmt.Println("  Contact:", contact(s, now))
		joined := "  Joined: " + s.Joined.Format(time.RFC3339)
		if s.Address != "" {
			joined += " from " + s.Address
		}
		fmt.Println(joined)
		if len(s.NodeGroups) > 0 {
			fmt.Println("  Node groups:", strings.Join(s.NodeGroups, ", "))
		} else {
			fmt.Println("  Node groups: none")
		}
		if !s.Replaced.IsZero() {
			fmt.Println("  Replaced a silent node of the same name at", s.Replaced.Format(time.RFC3339))
		}
		if s.OutageAlerter {
			fmt.Println("  Watches the hub" + outageAlerterPreference(s))
		} else if s.PreferOutageAlerter {
			fmt.Println("  Prefers to watch the hub (prefer_this_node_as_hub_outage_alerter); another preferred node has the role")
		}
		if s.ClockOffset != 0 {
			fmt.Printf("  Clock offset: %s\n", s.ClockOffset.Round(time.Second))
		}
		if s.Conflict != nil {
			fmt.Printf("  Name conflict: a node at %s claimed this name at %s\n", s.Conflict.Address, s.Conflict.At.Format(time.RFC3339))
		}
		if r := s.NodeStatusReport; r != nil {
			fmt.Printf("  Build: mmwatchdog %s (%s, %s/%s); configuration %.12s\n", r.Build.Version, r.Build.Revision, r.Build.OS, r.Build.Arch, r.Config)
			fmt.Println("  Node resources:", nodeResourceLine(r.NodeResources))
			fmt.Println("  " + r.Delivery.Describe())
			// The latest error is printed on its own only when no failing
			// destination shows it: it can be the mmhotline destination's.
			lastErrorShown := r.Delivery.LastError == ""
			for _, failing := range r.Delivery.FailingDestinations {
				fmt.Println("  " + failing.Describe())
				lastErrorShown = lastErrorShown || failing.ShowsError(r.Delivery.LastError)
			}
			if !lastErrorShown {
				fmt.Println("  Last delivery error:", r.Delivery.LastError)
			}
			if r.StorageError != "" {
				fmt.Println("  Storage:", r.StorageError)
			}
			for _, n := range checksandalerts.Names(r.Maintenance) {
				fmt.Printf("  Maintenance: %s %s\n", n, r.Maintenance[n].Describe())
			}
			checkNames := checksandalerts.Names(r.Checks)
			width := nameColumnWidth(checkNames, 28)
			for _, key := range checkNames {
				c := r.Checks[key]
				fmt.Printf("%-*s %-12s %s\n", width, key, c.State, c.Message)
				// An every_systemd_unit check has one line for each failed unit.
				for _, unit := range checksandalerts.Names(c.Findings) {
					u := c.Findings[unit]
					message := u.Message
					for _, a := range s.SetAsideFindings {
						if a.Check == key && a.Finding == unit {
							message = "set aside (" + a.Reason + ") until the exact finding changes"
						}
					}
					fmt.Printf("  %-26s %-12s %s\n", unit, u.State, message)
				}
				if c.FailureIncident != nil {
					fmt.Println("  Failure evidence:")
					printEvidence(c.FailureIncident.Evidence)
				}
				if c.CannotCheckIncident != nil {
					fmt.Println("  Cannot-check evidence:")
					printEvidence(c.CannotCheckIncident.Evidence)
				}
				for _, unit := range checksandalerts.Names(c.Findings) {
					if t := c.Findings[unit].FailureIncident; t != nil {
						fmt.Printf("  Open failure incident of %s, alert severity %s:\n", unit, t.AlertSeverity)
						printEvidence(t.Evidence)
					}
				}
			}
		}
		if len(s.Events) > 0 {
			fmt.Println("Recent events:")
		}
		for _, ev := range s.Events {
			fmt.Printf("%s %-20s %s %s\n", ev.At.Format(time.RFC3339), ev.Type, subjectOfEvent(ev), ev.Message)
		}
	}
	if s.ExitCode != 0 {
		return exitStatus(s.ExitCode)
	}
	return nil
}

// printFindings shows every node with a finding of one check: each finding,
// then each exact finding with the nodes that have it.
func printFindings(b []byte, asJSON bool) error {
	var found fleet.FindingsOfCheck
	if e := json.Unmarshal(b, &found); e != nil {
		return e
	}
	if asJSON {
		return printJSON(found)
	}
	found = cleaned(found)
	nodes := map[string]bool{}
	for _, f := range found.Findings {
		for _, n := range f.Nodes {
			nodes[n.Node] = true
		}
	}
	fmt.Printf("%s: %s on %s\n", found.Check, countOf(len(found.Findings), "finding"), countOf(len(nodes), "node"))
	for _, f := range found.Findings {
		fmt.Println(f.Finding)
		for i, n := range f.Nodes {
			if i == 0 || n.ExactFinding != f.Nodes[i-1].ExactFinding {
				exact, count := n.ExactFinding, 0
				for _, other := range f.Nodes[i:] {
					if other.ExactFinding == exact {
						count++
					}
				}
				if exact == "" {
					exact = "no exact finding"
				}
				fmt.Printf("  %s: %s\n", exact, countOf(count, "node"))
			}
			line := fmt.Sprintf("    %-24s %-12s %-5s", n.Node, n.State, n.AlertSeverity)
			if !n.FailingSince.IsZero() {
				line += " failing since " + n.FailingSince.Format(time.RFC3339)
			}
			if n.NodeState != "reporting" {
				line += " (node " + n.NodeState + ")"
			}
			if n.SetAside {
				line += " set aside (" + n.SetAsideReason + ")"
			}
			fmt.Println(strings.TrimRight(line, " "))
		}
	}
	return nil
}

// printSetAsideFindings shows each set-aside entry in force, with what it
// covers now.
func printSetAsideFindings(b []byte, asJSON bool) error {
	var listing struct {
		SetAsideFindings []fleet.SetAsideFindingNow `json:"set_aside_findings"`
	}
	if e := json.Unmarshal(b, &listing); e != nil {
		return e
	}
	if asJSON {
		return printJSON(listing)
	}
	listing = cleaned(listing)
	if len(listing.SetAsideFindings) == 0 {
		fmt.Println("No finding is set aside: the hub has no entry in /etc/mmwatchdog/set_aside_findings.yml.")
		return nil
	}
	for i, e := range listing.SetAsideFindings {
		if i > 0 {
			fmt.Println()
		}
		fmt.Printf("%s: %s\n", e.Check, e.Finding)
		fmt.Println("  Exact finding:", e.ExactFinding)
		fmt.Println("  Reason:", e.Reason)
		fmt.Println("  Investigation note:", e.InvestigationNote)
		fmt.Printf("  Set aside by %s on %s\n", e.SetAsideBy, e.SetAsideOn)
		covers := "  Covers " + countOf(e.CoveredNodeCount, "node") + " now"
		if len(e.CoveredNodes) > 0 {
			covers += ": " + strings.Join(e.CoveredNodes, ", ")
			if more := e.CoveredNodeCount - len(e.CoveredNodes); more > 0 {
				covers += fmt.Sprintf(", and %d more", more)
			}
		}
		fmt.Println(covers)
		if e.MatchesNoNodeNow {
			fmt.Println("  Matches no node now")
		} else {
			fmt.Println("  Matches a node now")
		}
		fmt.Printf("  Nodes that cannot check now: %d\n", e.NodesThatCannotCheckNow)
	}
	return nil
}

// cleaned returns a copy of v with the control characters in every string
// replaced, so text a node sent cannot write terminal escapes. JSON output
// relies on JSON escaping instead.
func cleaned[T any](v T) T {
	return clean(reflect.ValueOf(v)).Interface().(T)
}
func clean(v reflect.Value) reflect.Value {
	// Only strings and containers need cleaning; all other values are returned unchanged.
	//exhaustive:ignore
	switch v.Kind() {
	case reflect.String:
		return reflect.ValueOf(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return unicode.ReplacementChar
			}
			return r
		}, v.String())).Convert(v.Type())
	case reflect.Pointer:
		if v.IsNil() {
			return v
		}
		p := reflect.New(v.Type().Elem())
		p.Elem().Set(clean(v.Elem()))
		return p
	case reflect.Struct:
		c := reflect.New(v.Type()).Elem()
		c.Set(v)
		for i := 0; i < c.NumField(); i++ {
			if c.Field(i).CanSet() {
				c.Field(i).Set(clean(v.Field(i)))
			}
		}
		return c
	case reflect.Slice:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeSlice(v.Type(), v.Len(), v.Len())
		for i := 0; i < v.Len(); i++ {
			c.Index(i).Set(clean(v.Index(i)))
		}
		return c
	case reflect.Map:
		if v.IsNil() {
			return v
		}
		c := reflect.MakeMapWithSize(v.Type(), v.Len())
		for it := v.MapRange(); it.Next(); {
			c.SetMapIndex(clean(it.Key()), clean(it.Value()))
		}
		return c
	}
	return v
}

// countOf writes a count with its noun in the right number form: "1 node",
// "2 nodes", "0 nodes".
func countOf(count int, noun string) string {
	if count == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", count, noun)
}
