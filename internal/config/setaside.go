package config

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mmwatchdog/internal/checksandalerts"
)

// SetAsideFindingsFile is the hub's optional file of findings set aside
// after an investigation, beside the node settings. Only a fleet hub may
// have it: joined nodes never set anything aside, so a hub outage can only
// add alerts. The rules are in docs/changing-mmwatchdog/design/fleet.md
// under "Set a finding aside".
const SetAsideFindingsFile = "set_aside_findings.yml"

// SetAsideFinding is one entry of set_aside_findings.yml: one exact finding
// of one finding of one check, set aside after an investigation. Every
// field is required: an entry always names one exact finding, and says why,
// where the investigation is written down, who decided, and when.
type SetAsideFinding struct {
	// Check is the check, as CHECK_GROUP/CHECK.
	Check             string       `yaml:"check" json:"check"`
	Finding           string       `yaml:"finding" json:"finding"`
	ExactFinding      string       `yaml:"exact_finding" json:"exact_finding"`
	Reason            string       `yaml:"reason" json:"reason"`
	InvestigationNote string       `yaml:"investigation_note" json:"investigation_note"`
	SetAsideBy        string       `yaml:"set_aside_by" json:"set_aside_by"`
	SetAsideOn        SetAsideDate `yaml:"set_aside_on" json:"set_aside_on"`
}

// SetAsideDate is the date of a set-aside entry, such as 2026-11-23. YAML
// reads an unquoted date as a timestamp, so the date may be written with or
// without quotes.
type SetAsideDate string

// Key identifies the entry: two entries with the same check, finding, and
// exact finding are one entry. The hub records it on each node incident
// that the entry covers.
func (f SetAsideFinding) Key() string {
	return f.Check + "\n" + f.Finding + "\n" + f.ExactFinding
}

// SetAsideFindings are the entries of set_aside_findings.yml.
type SetAsideFindings []SetAsideFinding

// Covering returns the entry that covers a node incident of the check
// (CHECK_GROUP/CHECK), with the finding, its exact finding, and the
// incident's alert severity: the entry whose check, finding, and exact
// finding are all equal. An entry never covers an incident at sos, and a
// finding without an exact finding is never covered.
func (s SetAsideFindings) Covering(check, finding, exactFinding, severity string) (SetAsideFinding, bool) {
	if severity == "sos" {
		return SetAsideFinding{}, false
	}
	return s.Matching(check, finding, exactFinding)
}

// Matching returns the entry whose check, finding, and exact finding are
// all equal to these, whatever the severity.
func (s SetAsideFindings) Matching(check, finding, exactFinding string) (SetAsideFinding, bool) {
	if finding == "" || exactFinding == "" {
		return SetAsideFinding{}, false
	}
	for _, f := range s {
		if f.Check == check && f.Finding == finding && f.ExactFinding == exactFinding {
			return f, true
		}
	}
	return SetAsideFinding{}, false
}

// readSetAsideFindings reads set_aside_findings.yml of the configuration
// directory, when it exists. A node without fleet_hub_role refuses it.
func (c *Config) readSetAsideFindings(directory string) error {
	path := filepath.Join(directory, SetAsideFindingsFile)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	if c.FleetHubRole == nil {
		return fmt.Errorf("%s: only a fleet hub sets findings aside, and this node has no fleet_hub_role setting; remove the file from this node", path)
	}
	var file struct {
		SetAsideFindings *SetAsideFindings `yaml:"set_aside_findings"`
	}
	sources := map[string]checksandalerts.SourceLocation{}
	err := readYAML(path, &file, sources, nil)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if file.SetAsideFindings == nil {
		return fmt.Errorf("%s: expected a set_aside_findings: list; write \"set_aside_findings: []\" to set no finding aside", path)
	}
	for key, location := range sources {
		if key != "" {
			c.Sources[key] = location
		}
	}
	c.SetAsideFindings = *file.SetAsideFindings
	return c.validateSetAsideFindings()
}

// validateSetAsideFindings checks every entry: each field is written and
// not empty, the check and the finding have the form that a node's check
// and finding have, the exact finding is one that a module can supply, and
// no two entries are the same.
func (c *Config) validateSetAsideFindings() error {
	first := map[string]int{}
	for i, f := range c.SetAsideFindings {
		at := fmt.Sprintf("set_aside_findings.%d", i)
		for _, field := range []struct{ name, value, meaning string }{
			{"check", f.Check, "the check, as CHECK_GROUP/CHECK"},
			{"finding", f.Finding, "the finding's id, as mmwatchdog fleet findings shows it"},
			{"exact_finding", f.ExactFinding, "the finding's exact finding, copied whole from mmwatchdog fleet findings"},
			{"reason", f.Reason, "why the finding needs no action"},
			{"investigation_note", f.InvestigationNote, "where the investigation is written down"},
			{"set_aside_by", f.SetAsideBy, "who set the finding aside"},
			{"set_aside_on", string(f.SetAsideOn), "the date, such as 2026-11-23"},
		} {
			if strings.TrimSpace(field.value) == "" {
				return c.ErrorAt(at+"."+field.name, "is required and must not be empty: "+field.meaning)
			}
		}
		group, check, _ := strings.Cut(f.Check, "/")
		if !checksandalerts.NamePattern.MatchString(group) || !checksandalerts.NamePattern.MatchString(check) {
			return c.ErrorAt(at+".check", "expected CHECK_GROUP/CHECK, such as installed_packages/known_exploited_flaws")
		}
		if !checksandalerts.ValidFindingID(f.Finding) {
			return c.ErrorAt(at+".finding", fmt.Sprintf("expected a finding's id: text on one line of at most %d bytes", checksandalerts.FindingIDLimit))
		}
		if !checksandalerts.ValidExactFinding(f.ExactFinding) {
			return c.ErrorAt(at+".exact_finding", fmt.Sprintf("expected an exact finding: text on one line of at most %d bytes", checksandalerts.ExactFindingLimit))
		}
		if earlier, seen := first[f.Key()]; seen {
			return c.ErrorAt(at, fmt.Sprintf("sets aside the same check, finding, and exact finding as set_aside_findings.%d; keep one of the two entries", earlier))
		}
		first[f.Key()] = i
	}
	return nil
}

// validSetAsideDate reports a date such as 2026-11-23.
func validSetAsideDate(value string) bool {
	_, err := time.Parse(time.DateOnly, value)
	return err == nil
}
