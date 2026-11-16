package checksandalerts

import (
	"bufio"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// ZypperSecurityPatches is the inputs of the zypper_security_patches check
// module, which fails while a security patch waits to be installed on a
// SUSE node. The check reads what zypper already knows and downloads
// nothing.
type ZypperSecurityPatches struct {
	FailWhen string `yaml:"fail_when" json:"fail_when"`
}

func (n *ZypperSecurityPatches) Validate(v InputValidation) *FieldError {
	inputs := map[string]any{}
	if n.FailWhen != "" || v.Authored != nil && v.Authored("fail_when") {
		inputs["fail_when"] = n.FailWhen
	}
	return zypperSecurityPatchesDescription.InputsProblem("zypper_security_patches", inputs)
}

func (n *ZypperSecurityPatches) Run(ctx context.Context, _ *CheckRun) Observation {
	family, name := primitives.NodeDistributionFamily()
	name = namedOrUnnamed(name)
	switch family {
	case primitives.SUSEFamily:
	case primitives.DebianFamily:
		return Result("unknown", "this node runs "+name+", whose package manager is apt, not zypper: use the check apt_security_updates")
	case primitives.RedHatFamily:
		return Result("unknown", "this node runs "+name+", whose package manager is dnf, not zypper: use the check dnf_security_updates")
	default:
		return unknownPackageManager(name)
	}
	if problem := zypperMetadataProblem(); problem != nil {
		return *problem
	}
	// Run as the unprivileged account, zypper cannot build a missing cache,
	// so the check never downloads anything.
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: []string{"zypper", "--non-interactive", "--no-refresh", "--xmlout", "list-patches", "--category", "security"}, RunAs: &primitives.ExecOptions{RunAsUserAccount: primitives.UnprivilegedAccount, RunFromDirectory: "/", Env: map[string]string{"LC_ALL": "C"}}, StandardOutputLimit: primitives.OwnStandardOutputLimit})
	var notFound *exec.Error
	if errors.As(err, &notFound) {
		return Result("unknown", primitives.DetailLine("MMWatchdog cannot run zypper: "+notFound.Err.Error()))
	}
	if err != nil {
		return Result("unknown", primitives.DetailLine("MMWatchdog cannot ask zypper which patches wait: "+err.Error()))
	}
	patches, errorTexts, err := readZypperPatchList(ran.StandardOutput)
	// Exit code 106 means that zypper skipped a repository; its error text
	// names the repository. Any other non-zero code is an error too.
	if ran.ExitCode != 0 || len(errorTexts) > 0 {
		reason := strings.Join(errorTexts, "; ")
		if reason == "" {
			reason = strings.TrimSpace(ran.StandardError)
		}
		if reason == "" {
			reason = fmt.Sprintf("zypper ended with exit code %d", ran.ExitCode)
		}
		return Result("unknown", primitives.DetailLine("MMWatchdog cannot ask zypper which patches wait: "+primitives.Clip(reason, 300)))
	}
	if err != nil {
		return Result("unknown", primitives.DetailLine("MMWatchdog cannot read what zypper wrote: "+err.Error()))
	}
	if len(patches) == 0 {
		return Result("ok", "no security patch waits to be installed")
	}
	described := make([]string, len(patches))
	for index, patch := range patches {
		described[index] = patch.name + " (" + patch.severity + "): " + patch.summary
		if patch.needsReboot {
			described[index] += " (needs a reboot)"
		}
	}
	return Result("failed", countOf(len(patches), "security patch waits", "security patches wait")+" to be installed: "+shownNames(described))
}

// zypperPatch is a patch in the XML of zypper list-patches.
type zypperPatch struct {
	name, severity, summary string
	needsReboot             bool
}

// readZypperPatchList reads zypper's XML. The patches are the update
// elements of kind patch, with status needed and category security,
// sorted by name, each once. errorTexts are the texts of the message
// elements of type error.
func readZypperPatchList(written string) (patches []zypperPatch, errorTexts []string, err error) {
	decoder := xml.NewDecoder(strings.NewReader(written))
	seen := map[string]bool{}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errorTexts, err
		}
		start, isStart := token.(xml.StartElement)
		if !isStart {
			continue
		}
		attribute := func(name string) string {
			for _, found := range start.Attr {
				if found.Name.Local == name {
					return found.Value
				}
			}
			return ""
		}
		switch start.Name.Local {
		case "message":
			var text struct {
				Text string `xml:",chardata"`
			}
			if err := decoder.DecodeElement(&text, &start); err != nil {
				return nil, errorTexts, err
			}
			if attribute("type") == "error" {
				errorTexts = append(errorTexts, strings.TrimSpace(text.Text))
			}
		case "update":
			var update struct {
				Summary string `xml:"summary"`
			}
			if err := decoder.DecodeElement(&update, &start); err != nil {
				return nil, errorTexts, err
			}
			if attribute("kind") == "patch" && attribute("status") == "needed" && attribute("category") == "security" && !seen[attribute("name")] {
				seen[attribute("name")] = true
				patches = append(patches, zypperPatch{name: attribute("name"), severity: attribute("severity"), summary: strings.TrimSpace(update.Summary), needsReboot: attribute("restart") == "true"})
			}
		}
	}
	slices.SortFunc(patches, func(a, b zypperPatch) int { return strings.Compare(a.name, b.name) })
	return patches, errorTexts, nil
}

// zypperRefresh says how zypper comes to refresh its metadata every day.
// openSUSE Leap's cloud image refreshes nothing by itself.
const zypperRefresh = "set up a daily refresh, a systemd timer that runs zypper --non-interactive refresh, as the page of zypper_security_patches shows (mmwatchdog describe-module zypper_security_patches)"

// enabledRepositories reads the repository files that match the pattern and
// returns the names of the enabled repositories: the aliases of zypper's
// /etc/zypp/repos.d/*.repo, or the ids of dnf's /etc/yum.repos.d/*.repo,
// which have the same form. A file holds one or more sections, each a
// repository with its name as the section name. A repository without
// enabled= is enabled, for zypper and for dnf. The alias is also the name of the
// repository's directory in zypper's cache, /var/cache/zypp/raw: the alias
// of a repository of a service already carries the service, such as
// openSUSE:repo-oss, so the service= key is not needed. A disabled
// repository has no directory.
func enabledRepositories(filePattern string) ([]string, error) {
	files, err := filepath.Glob(primitives.NodeFile(filePattern))
	if err != nil {
		return nil, err
	}
	slices.Sort(files)
	var aliases []string
	for _, file := range files {
		data, err := primitives.ReadFile(file, primitives.ProcFileLimit)
		if err != nil {
			return nil, err
		}
		alias, enabled := "", true
		finish := func() {
			if alias != "" && enabled {
				aliases = append(aliases, alias)
			}
		}
		lines := bufio.NewScanner(strings.NewReader(string(data)))
		for lines.Scan() {
			line := strings.TrimSpace(lines.Text())
			if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
				finish()
				alias, enabled = line[1:len(line)-1], true
				continue
			}
			if key, value, found := strings.Cut(line, "="); found && strings.TrimSpace(key) == "enabled" {
				enabled = !slices.Contains([]string{"0", "no", "false", "off"}, strings.ToLower(strings.TrimSpace(value)))
			}
		}
		finish()
	}
	return aliases, nil
}

// zypperMetadataProblem requires, for each enabled repository, the file
// repodata/repomd.xml in zypper's cache, not older than seven days: zypper
// writes the file only when a refresh of that repository succeeds, so the
// oldest file decides. A missing or old file makes the check unknown,
// because without fresh metadata, "no patch waits" would be a guess.
func zypperMetadataProblem() *Observation {
	aliases, err := enabledRepositories("/etc/zypp/repos.d/*.repo")
	if err != nil {
		o := Result("unknown", primitives.DetailLine("MMWatchdog cannot read the repositories of zypper in /etc/zypp/repos.d: "+err.Error()))
		return &o
	}
	if len(aliases) == 0 {
		o := Result("unknown", "zypper has no enabled repository in /etc/zypp/repos.d, so MMWatchdog cannot know which patches wait")
		return &o
	}
	var oldest *Observation
	var oldestAge time.Duration
	for _, alias := range aliases {
		path := "/var/cache/zypp/raw/" + alias + "/repodata/repomd.xml"
		about, err := primitives.InformationAboutFile(primitives.NodeFile(path))
		if err != nil || !about.IsRegularFile {
			o := Result("unknown", "the repository "+alias+" has no usable "+path+", which zypper writes after a successful refresh of the repository, so MMWatchdog cannot know which patches wait: "+zypperRefresh)
			return &o
		}
		age := time.Since(about.Modified)
		if age > packageListsTooOldAfter && age > oldestAge {
			oldestAge = age
			o := Result("unknown", fmt.Sprintf("%s, which zypper writes after a successful refresh of the repository %s, is %d days old, so MMWatchdog cannot know which patches wait: %s", path, alias, int(age.Hours()/24), zypperRefresh))
			oldest = &o
		}
	}
	return oldest
}

// zypperSecurityPatchesDescription is what "mmwatchdog describe-module zypper_security_patches" shows, and what
// validation knows about the module's inputs.
var zypperSecurityPatchesDescription = ModuleDescription{
	Summary: "Fails while a security patch waits to be installed on a SUSE node, such as openSUSE Leap. The check reads what zypper already knows, and downloads nothing.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "fail_when", Type: "string", Required: true, AllowedValues: []any{"a_security_patch_waits_to_be_installed"}, Description: "The check fails while a security patch waits to be installed; write a_security_patch_waits_to_be_installed, the only value."},
	},
	Examples: []ModuleExample{{
		Description:   "Tell the team when a security patch waits to be installed.",
		Configuration: "zypper_security_patches:\n  fail_when: a_security_patch_waits_to_be_installed\nalert_severity: warn\n",
	}},
}
