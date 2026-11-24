package checksandalerts

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os/exec"
	"slices"
	"strings"

	"mmwatchdog/internal/primitives"
)

// UbuntuProSecurityFixes reports fixes that the node cannot get without an
// Ubuntu Pro service. Each source package is one finding.
type UbuntuProSecurityFixes struct {
	FailWhen string `yaml:"fail_when" json:"fail_when"`
}

func (n *UbuntuProSecurityFixes) Validate(v InputValidation) *FieldError {
	inputs := map[string]any{}
	if n.FailWhen != "" || v.Authored != nil && v.Authored("fail_when") {
		inputs["fail_when"] = n.FailWhen
	}
	return ubuntuProSecurityFixesDescription.InputsProblem("ubuntu_pro_security_fixes", inputs)
}

// ubuntuProSecurityStatus keeps presence separate from false and an empty
// package list: missing fields must never produce a healthy observation.
type ubuntuProSecurityStatus struct {
	Packages []struct {
		Package     string `json:"package"`
		Version     string `json:"version"`
		ServiceName string `json:"service_name"`
		Status      string `json:"status"`
	} `json:"packages"`
	Summary struct {
		UA struct {
			Attached *bool `json:"attached"`
		} `json:"ua"`
	} `json:"summary"`
}

func (n *UbuntuProSecurityFixes) Run(ctx context.Context, _ *CheckRun) Observation {
	family, name := primitives.NodeDistributionFamily()
	release := primitives.NodeOSRelease()
	ubuntu := release["ID"] == "ubuntu"
	for _, id := range strings.Fields(release["ID_LIKE"]) {
		ubuntu = ubuntu || id == "ubuntu"
	}
	if family != primitives.DebianFamily || !ubuntu {
		return Result("unknown", "Ubuntu Pro exists only on Ubuntu, and this node runs "+namedOrUnnamed(name))
	}
	lists := hasNonemptyPackageLists("/var/lib/apt/lists/*_Packages", "/var/lib/apt/lists/*_Packages.*")
	if problem := packageListsProblem("apt", aptRefresh, lists, "/var/lib/apt/periodic/update-success-stamp"); problem != nil {
		return *problem
	}
	run := func(arguments ...string) (primitives.CommandResult, error) {
		return primitives.RunCommand(ctx, primitives.Command{Arguments: arguments, RunAs: &primitives.ExecOptions{RunAsUserAccount: primitives.UnprivilegedAccount, RunFromDirectory: "/", Env: map[string]string{"LC_ALL": "C"}}, StandardOutputLimit: primitives.OwnStandardOutputLimit})
	}
	ran, err := run("pro", "security-status", "--format", "json")
	var missing *exec.Error
	if errors.As(err, &missing) {
		return Result("unknown", "MMWatchdog cannot run pro, the command of Ubuntu's package ubuntu-pro-client: "+missing.Err.Error()+": install ubuntu-pro-client")
	}
	if problem := packageManagerProblem("pro", ran, err); problem != nil {
		return *problem
	}
	var status ubuntuProSecurityStatus
	if err := json.Unmarshal([]byte(ran.StandardOutput), &status); err != nil || status.Packages == nil || status.Summary.UA.Attached == nil {
		return Result("unknown", "MMWatchdog cannot read pro security-status JSON: expected a packages array and summary.ua.attached boolean")
	}
	if !*status.Summary.UA.Attached && !hasNonemptyPackageLists("/var/lib/ubuntu-advantage/apt-esm/var/lib/apt/lists/*_Packages") {
		return Result("unknown", "the Ubuntu Pro client has no lists of ESM fixes in /var/lib/ubuntu-advantage/apt-esm, so MMWatchdog cannot know which fixes only Ubuntu Pro delivers: run apt-get update as root, which starts the Ubuntu Pro client's esm-cache.service")
	}
	inventory, err := run("dpkg-query", "--show", "--showformat=${db:Status-Status}\t${Package}\t${source:Package}\t${source:Version}\n")
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read installed packages with dpkg-query: "+err.Error())
	}
	if inventory.ExitCode != 0 {
		return Result("unknown", "MMWatchdog cannot read installed packages with dpkg-query: "+primitives.Clip(strings.TrimSpace(inventory.StandardError), 300))
	}
	// The source package of each installed binary package, and the lowest
	// installed version of each source package.
	sources, installedVersions := map[string]string{}, map[string]string{}
	for _, line := range strings.Split(inventory.StandardOutput, "\n") {
		fields := strings.Split(line, "\t")
		if len(fields) == 4 && fields[0] == "installed" && fields[2] != "" {
			sources[fields[1]] = fields[2]
			if old, found := installedVersions[fields[2]]; !found || primitives.CompareDebianVersions(fields[3], old) < 0 {
				installedVersions[fields[2]] = fields[3]
			}
		}
	}
	// Source -> service -> binary -> fixed version. Keeping services separate
	// also explains a source whose binaries need different Pro services.
	fixes := map[string]map[string]map[string]string{}
	for _, p := range status.Packages {
		if (p.ServiceName != "esm-apps" && p.ServiceName != "esm-infra") || p.Status == "upgrade_available" {
			continue
		}
		if p.Package == "" || p.Version == "" || p.Status == "" {
			return Result("unknown", "MMWatchdog cannot read pro security-status JSON: an ESM fix has no package, version, or status")
		}
		source := sources[p.Package]
		if source == "" {
			source = p.Package
		}
		if fixes[source] == nil {
			fixes[source] = map[string]map[string]string{}
		}
		if fixes[source][p.ServiceName] == nil {
			fixes[source][p.ServiceName] = map[string]string{}
		}
		fixes[source][p.ServiceName][p.Package] = p.Version
	}
	names := slices.Sorted(maps.Keys(fixes))
	observation := Result("ok", "this node misses no security fix that only Ubuntu Pro delivers")
	if len(names) == 0 {
		return observation
	}
	observation = Result("failed", countOf(len(names), "source package has", "source packages have")+" a security fix that this node misses because only Ubuntu Pro delivers it: "+shownNames(names))
	releaseName := releaseOfTheNode()
	for _, source := range names {
		messages := []string{}
		// The exact finding: the release, the source package, its installed
		// version, and each service with the fixed versions it delivers,
		// such as "ubuntu 24.04 python-wheel 0.42.0-2; esm-apps fixed
		// version: 0.42.0-2ubuntu0.1~esm1".
		installed := cmp.Or(installedVersions[source], "installed version unknown")
		exact := []string{releaseName + " " + source + " " + installed}
		for _, service := range []string{"esm-apps", "esm-infra"} {
			binaries := fixes[source][service]
			if len(binaries) == 0 {
				continue
			}
			packages := slices.Sorted(maps.Keys(binaries))
			versions := uniqueSorted(slices.Collect(maps.Values(binaries)))
			exact = append(exact, service+" fixed version: "+strings.Join(versions, ", "))
			detail := "version " + versions[0] + " of the installed package"
			if len(versions) > 1 {
				for i, binary := range packages {
					packages[i] = binary + " " + binaries[binary]
				}
				detail = "the installed package"
			}
			if len(packages) > 1 {
				detail += "s"
			}
			detail += " " + strings.Join(packages, ", ")
			label := "ESM Apps"
			if service == "esm-infra" {
				label = "ESM Infra"
			}
			messages = append(messages, source+" has a security fix that this node misses because only Ubuntu Pro's "+label+" delivers it: "+detail)
		}
		observation.Findings = append(observation.Findings, Finding{ID: source, Message: strings.Join(messages, "; "), ExactFinding: FitExactFinding(strings.Join(exact, "; "))})
	}
	return observation
}

func ubuntuProSecurityFixIsGone(source string) string {
	return "this node no longer misses a security fix of " + source + " that only Ubuntu Pro delivers"
}

var ubuntuProSecurityFixesDescription = ModuleDescription{
	Summary:                   "Fails while the node misses a security fix of an installed package because only an Ubuntu Pro service that the node does not get delivers the fix. The check reads what the Ubuntu Pro client on the node already knows, and downloads nothing.",
	UsedAs:                    UsedAsCheck,
	OneIncidentForEachFinding: true,
	Inputs:                    []ModuleInput{{Name: "fail_when", Type: "string", Required: true, AllowedValues: []any{"the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers"}, Description: "The check fails while the node misses a security fix that only Ubuntu Pro delivers; write the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers, the only value."}},
	Examples:                  []ModuleExample{{Description: "Tell the team when the node misses a security fix that only Ubuntu Pro delivers.", Configuration: "ubuntu_pro_security_fixes:\n  fail_when: the_node_misses_a_security_fix_that_only_ubuntu_pro_delivers\nalert_severity: warn\n"}},
}
