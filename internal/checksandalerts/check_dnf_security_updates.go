package checksandalerts

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// DnfSecurityUpdates is the inputs of the dnf_security_updates check
// module, which fails while a security update waits to be installed on a
// node of the Red Hat family, Fedora included. The check reads what dnf
// already knows and downloads nothing.
type DnfSecurityUpdates struct {
	FailWhen string `yaml:"fail_when" json:"fail_when"`
}

func (n *DnfSecurityUpdates) Validate(v InputValidation) *FieldError {
	inputs := map[string]any{}
	if n.FailWhen != "" || v.Authored != nil && v.Authored("fail_when") {
		inputs["fail_when"] = n.FailWhen
	}
	return dnfSecurityUpdatesDescription.InputsProblem("dnf_security_updates", inputs)
}

func (n *DnfSecurityUpdates) Run(ctx context.Context, _ *CheckRun) Observation {
	family, name := primitives.NodeDistributionFamily()
	name = namedOrUnnamed(name)
	switch family {
	case primitives.RedHatFamily:
	case primitives.DebianFamily:
		return Result("unknown", "this node runs "+name+", whose package manager is apt, not dnf: use the check apt_security_updates")
	case primitives.SUSEFamily:
		return Result("unknown", "this node runs "+name+", whose package manager is zypper, not dnf: use the check zypper_security_patches")
	default:
		return unknownPackageManager(name)
	}
	dnf := dnfOfTheNode(ctx)
	if problem := dnf.metadataProblem(ctx); problem != nil {
		return *problem
	}
	// dnf's cache belongs to root. dnf 4 run as the unprivileged account
	// reads a cache of its own, which is empty, and --cacheonly then ends
	// with exit code 0 and no output: "no update waits" from no data. So dnf
	// runs as MMWatchdog itself, which is root; --cacheonly keeps it from
	// fetching anything. LC_ALL=C gives dnf's own words, whatever the language
	// of the node. --updates: only the advisories for which dnf has an update
	// that it would install. Without it, dnf also lists an advisory for a
	// package of a module stream that the node does not use, such as redis 7
	// on a node with redis 6: an update that never comes, so the check would
	// never be healthy again.
	arguments := append([]string{"env", "LC_ALL=C", "dnf", "--quiet", "--cacheonly", "updateinfo", "list", "--security", "--updates"}, dnf.listingOptions...)
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: arguments, AsMMWatchdogItself: true})
	if problem := packageManagerProblem("dnf", ran, err); problem != nil {
		return *problem
	}
	// An error that dnf 4 writes beside an exit code of 0, such as
	// "Error: Cache-only enabled but no cache for 'baseos'".
	if said := whatTheClientSays(ran.StandardError); strings.HasPrefix(said, "Error:") {
		return Result("unknown", primitives.DetailLine("MMWatchdog cannot ask dnf which updates wait: "+said))
	}
	updates, understood := dnf.updatesIn(ran.StandardOutput)
	if !understood {
		return Result("unknown", primitives.DetailLine("dnf gave a list of security updates that MMWatchdog does not understand: "+primitives.Clip(strings.TrimSpace(ran.StandardOutput), 200)))
	}
	if len(updates) == 0 {
		return Result("ok", "no security update waits to be installed")
	}
	// One finding for each advisory: the distribution's advisory is the
	// unit in which it ships a security fix, and is one problem on every
	// node that waits for it. The list names an advisory once for each of
	// its packages.
	severities, packages := map[string]string{}, map[string][]string{}
	for _, update := range updates {
		severities[update.advisory] = cmp.Or(severities[update.advisory], update.severity)
		packages[update.advisory] = append(packages[update.advisory], rpmPackageName(update.nevra))
	}
	advisories := slices.Sorted(maps.Keys(packages))
	o := Result("failed", countOf(len(advisories), "security advisory waits", "security advisories wait")+" to be installed: "+shownNames(advisories))
	releaseName := releaseOfTheNode()
	for _, advisory := range advisories {
		severity := cmp.Or(severities[advisory], "no severity")
		o.Findings = append(o.Findings, Finding{
			ID:           advisory,
			Message:      "the security advisory " + advisory + " (" + severity + ") waits to be installed: " + strings.Join(uniqueSorted(packages[advisory]), ", "),
			ExactFinding: FitExactFinding(releaseName + " " + advisory + " " + severity),
		})
	}
	return o
}

// dnfSecurityAdvisoryIsGone is the message of the run in which the security
// advisory no longer waits to be installed.
func dnfSecurityAdvisoryIsGone(advisory string) string {
	return "the security advisory " + advisory + " no longer waits to be installed"
}

// dnfAdvisoryPackage is one entry of dnf's list of security updates: an
// advisory, its severity as dnf names it, such as Important, or "" when
// dnf names none, and one package that it updates, with its version, as
// NAME-VERSION-RELEASE.ARCHITECTURE.
type dnfAdvisoryPackage struct{ advisory, severity, nevra string }

// dnfVersion is what differs between dnf 4 and dnf 5: how the check knows
// that the package data is fresh, which options give the list of security
// updates in a form that MMWatchdog reads, and how it reads that list. Each
// version keeps the rule that nothing is guessed: what a version's reader
// does not understand makes the check unknown.
type dnfVersion struct {
	metadataProblem func(ctx context.Context) *Observation
	listingOptions  []string
	updatesIn       func(written string) (updates []dnfAdvisoryPackage, understood bool)
	// rebootHint answers reboot_after_updates on the Red Hat family.
	rebootHint func(ctx context.Context) Observation
}

var (
	dnfFour = dnfVersion{
		metadataProblem: func(context.Context) *Observation { return dnfFourMetadataProblem() },
		updatesIn:       securityUpdatesInDNFFourList,
		rebootHint:      rebootHintOfDNF,
	}
	dnfFive = dnfVersion{
		metadataProblem: dnfFiveMetadataProblem,
		listingOptions:  []string{"--json"},
		updatesIn:       securityUpdatesInDNFFiveList,
		rebootHint:      rebootHintOfDNFFive,
	}
)

// dnfOfTheNode tells dnf 5 from dnf 4 by the node: on Fedora 41 and newer,
// the program dnf is a link to dnf5. A dnf5 installed beside a dnf 4 does
// not count, because the program dnf is the one that runs.
func dnfOfTheNode(ctx context.Context) dnfVersion {
	if primitives.ProgramIsLinkTo(ctx, "dnf", "dnf5") {
		return dnfFive
	}
	return dnfFour
}

// dnfRefresh says how the package data of dnf comes to be refreshed. The
// timer is enabled on Rocky Linux, AlmaLinux, and Fedora; a node can have it
// off.
const dnfRefresh = "enable the refresh timer of dnf, systemctl enable --now dnf-makecache.timer, as the page of dnf_security_updates shows (mmwatchdog describe-module dnf_security_updates)"

// dnfCacheDirectory is the name of the directory of one repository in dnf's
// cache: the repository's id, a hyphen, and 16 hexadecimal digits of a hash
// of the repository's address. The address changes with the configuration,
// so an old directory can remain beside the current one.
func dnfCacheDirectory(repository string) *regexp.Regexp {
	return regexp.MustCompile("^" + regexp.QuoteMeta(repository) + "-[0-9a-f]{16}$")
}

// dnfFourMetadataProblem requires, for each enabled repository, a file of
// package data in dnf's cache that a successful refresh of that repository
// touched at most seven days ago. The oldest repository decides. The
// repository's file of package data is the newest repodata/*primary.* of the
// directories of its id. dnf touches that file at each refresh that reaches
// the repository's server, also when the repository has not changed, and
// never at a refresh that fails. The stamp /var/cache/dnf/last_makecache
// does not do this: dnf makecache --timer, which dnf-makecache.timer runs,
// touches it also when the refresh fails, and a node whose refresh always
// fails would look fresh. A missing or old file makes the check unknown,
// because without fresh data, "no update waits" would be a guess.
func dnfFourMetadataProblem() *Observation {
	ids, err := enabledRepositories("/etc/yum.repos.d/*.repo")
	if err != nil {
		o := Result("unknown", primitives.DetailLine("MMWatchdog cannot read the repositories of dnf in /etc/yum.repos.d: "+err.Error()))
		return &o
	}
	if len(ids) == 0 {
		o := Result("unknown", "dnf has no enabled repository in /etc/yum.repos.d, so MMWatchdog cannot know which updates wait")
		return &o
	}
	files, _ := filepath.Glob(primitives.NodeFile("/var/cache/dnf/*/repodata/*primary.*"))
	var oldest *Observation
	var oldestAge time.Duration
	for _, id := range ids {
		matches := dnfCacheDirectory(id)
		var newestAge time.Duration
		var newestDirectory string
		for _, file := range files {
			directory := filepath.Base(filepath.Dir(filepath.Dir(file)))
			about, err := primitives.InformationAboutFile(file)
			if !matches.MatchString(directory) || err != nil || !about.IsRegularFile || about.SizeBytes == 0 {
				continue
			}
			if age := time.Since(about.Modified); newestDirectory == "" || age < newestAge {
				newestAge, newestDirectory = age, "/var/cache/dnf/"+directory
			}
		}
		if newestDirectory == "" {
			o := Result("unknown", "the repository "+id+" has no package data in dnf's cache (a file *primary.* in /var/cache/dnf/"+id+"-HASH/repodata), so MMWatchdog cannot know which updates wait: "+dnfRefresh)
			return &o
		}
		if newestAge > packageListsTooOldAfter && newestAge > oldestAge {
			oldestAge = newestAge
			o := Result("unknown", fmt.Sprintf("the package data of the repository %s (a file *primary.* in %s/repodata), which dnf touches at each successful refresh of the repository, is %d days old, so MMWatchdog cannot know which updates wait: %s", id, newestDirectory, int(newestAge.Hours()/24), dnfRefresh))
			oldest = &o
		}
	}
	return oldest
}

// dnfFiveMetadataProblem asks dnf 5 for its enabled repositories, in JSON,
// and requires, for each, a timestamp that is not older than the repository
// allows. dnf 5 knows the enabled repositories wherever their files are
// (Fedora keeps them in /usr/share/dnf5/repos.d, and a file of the same name
// in /etc/yum.repos.d replaces one), and their settings.
//
// The timestamp is the modification time of the repository's file of package
// data, *primary.*, in the directory of the repository's current address in
// dnf's cache; the file repomd.xml does not count. dnf 5 touches that file
// at each refresh that reaches the repository's server, also when the
// repository has not changed, and never at a refresh that fails. dnf 5
// refreshes a repository only when its data is older than the repository's
// own metadata_expire, which Fedora sets to 6 hours for the updates, 7 days
// for the release, and 14 days for the openh264 repository. A node whose
// refresh works therefore has data as old as metadata_expire plus the wait
// for the next run of the refresh timer. So the limit is the repository's
// metadata_expire plus one day, but at least the 7 days of the other package
// managers and at most dnfFiveRepositoryAgeCeiling. The oldest repository decides. A repository that cannot be read
// makes the check unknown, because without fresh data, "no update waits"
// would be a guess.
// dnfFiveRepositoryAgeCeiling is the most that a repository's own
// metadata_expire can raise the limit to. Without a ceiling, a repository
// with a metadata_expire of months would let the check report "no update
// waits" from data months old. Fedora's longest metadata_expire is 14 days,
// so 15 days covers every Fedora repository as Fedora ships it.
const dnfFiveRepositoryAgeCeiling = 15 * 24 * time.Hour

func dnfFiveMetadataProblem(ctx context.Context) *Observation {
	unknown := func(message string) *Observation {
		o := Result("unknown", message)
		return &o
	}
	ran, err := primitives.RunCommand(ctx, primitives.Command{Arguments: []string{"env", "LC_ALL=C", "dnf", "--quiet", "--cacheonly", "repo", "info", "--enabled", "--json"}, AsMMWatchdogItself: true})
	switch {
	case err != nil:
		return unknown(primitives.DetailLine("MMWatchdog cannot ask dnf about its repositories: " + err.Error()))
	case ran.ExitCode != 0:
		return unknown(primitives.DetailLine("MMWatchdog cannot ask dnf about its repositories: " + primitives.Clip(strings.TrimSpace(ran.StandardError), 300)))
	}
	var repositories []struct {
		ID                      *string `json:"id"`
		TimestampInUnixSeconds  *int64  `json:"timestamp"`
		MetadataExpireInSeconds *int64  `json:"metadata_expire"`
	}
	notUnderstood := func() *Observation {
		return unknown(primitives.DetailLine("dnf gave a list of repositories that MMWatchdog does not understand: " + primitives.Clip(strings.TrimSpace(ran.StandardOutput), 200)))
	}
	if json.Unmarshal([]byte(ran.StandardOutput), &repositories) != nil || repositories == nil {
		return notUnderstood()
	}
	if len(repositories) == 0 {
		return unknown("dnf has no enabled repository, so MMWatchdog cannot know which updates wait")
	}
	var oldest *Observation
	var oldestAge time.Duration
	for _, repository := range repositories {
		if repository.ID == nil || *repository.ID == "" || repository.TimestampInUnixSeconds == nil || repository.MetadataExpireInSeconds == nil {
			return notUnderstood()
		}
		if *repository.TimestampInUnixSeconds <= 0 {
			return unknown("the repository " + *repository.ID + " has no package data in dnf's cache, so MMWatchdog cannot know which updates wait: " + dnfRefresh)
		}
		limit, remedy := packageListsTooOldAfter, dnfRefresh
		if expire := time.Duration(*repository.MetadataExpireInSeconds) * time.Second; expire > 0 && expire+24*time.Hour > limit {
			limit = expire + 24*time.Hour
			if limit > dnfFiveRepositoryAgeCeiling {
				limit = dnfFiveRepositoryAgeCeiling
				remedy = fmt.Sprintf("the repository's metadata_expire of %d days is longer than MMWatchdog accepts for security data; set it to at most 14 days in the repository's file", int(expire.Hours()/24))
			}
		}
		if age := time.Since(time.Unix(*repository.TimestampInUnixSeconds, 0)); age > limit && age > oldestAge {
			oldestAge = age
			oldest = unknown(fmt.Sprintf("the package data of the repository %s, which dnf touches at each successful refresh of the repository, is %d days old (the limit for this repository is %d days), so MMWatchdog cannot know which updates wait: %s", *repository.ID, int(age.Hours()/24), int(limit.Hours()/24), remedy))
		}
	}
	return oldest
}

// securityUpdatesInDNFFourList reads the advisories and their packages
// from what dnf 4's "dnf updateinfo list --security" wrote: one line for
// each advisory and package. dnf has already left out every advisory that
// is no security advisory. dnf writes three columns: the advisory, its
// severity and kind, such as Important/Sec., and the package. A list with a
// line of another form is not understood.
func securityUpdatesInDNFFourList(written string) (updates []dnfAdvisoryPackage, understood bool) {
	updates = []dnfAdvisoryPackage{}
	for _, line := range strings.Split(written, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case len(fields) == 3:
			severity, _, named := strings.Cut(fields[1], "/")
			if !named {
				severity = ""
			}
			updates = append(updates, dnfAdvisoryPackage{advisory: fields[0], severity: severity, nevra: fields[2]})
		default:
			return nil, false
		}
	}
	return updates, true
}

// securityUpdatesInDNFFiveList reads the advisories and their packages
// from what dnf 5's "dnf updateinfo list --security --updates --json"
// wrote: a JSON array with one object for each advisory and package, with
// the advisory's name, type, and severity, and the package with its version
// in the field nevra. The array is empty when no update waits, while dnf 5's
// text list is then empty too and otherwise has a header line and a date
// with a space, which would need a guess at the columns. A list that is not
// an array, or has an entry without a name, without the type security, or
// without a package, is not understood.
func securityUpdatesInDNFFiveList(written string) (updates []dnfAdvisoryPackage, understood bool) {
	var advisories []struct {
		Name     *string `json:"name"`
		Type     *string `json:"type"`
		Severity string  `json:"severity"`
		Nevra    *string `json:"nevra"`
	}
	if json.Unmarshal([]byte(written), &advisories) != nil || advisories == nil {
		return nil, false
	}
	updates = []dnfAdvisoryPackage{}
	for _, advisory := range advisories {
		if advisory.Name == nil || *advisory.Name == "" || advisory.Type == nil || *advisory.Type != "security" || advisory.Nevra == nil || *advisory.Nevra == "" {
			return nil, false
		}
		updates = append(updates, dnfAdvisoryPackage{advisory: *advisory.Name, severity: advisory.Severity, nevra: *advisory.Nevra})
	}
	return updates, true
}

// rpmPackageName returns the name of a package from the way dnf writes a
// package with its version: NAME-VERSION-RELEASE.ARCHITECTURE, such as
// bind-libs-32:9.16.23-18.el9_4.1.x86_64. The name can hold hyphens; the
// version and the release cannot.
func rpmPackageName(withVersion string) string {
	name := withVersion
	for range 2 {
		end := strings.LastIndex(name, "-")
		if end <= 0 {
			return withVersion
		}
		name = name[:end]
	}
	return name
}

// dnfSecurityUpdatesDescription is what "mmwatchdog describe-module dnf_security_updates" shows, and what
// validation knows about the module's inputs.
var dnfSecurityUpdatesDescription = ModuleDescription{
	Summary:                   "Fails while a security update waits to be installed on a node of the Red Hat family, such as Rocky Linux, AlmaLinux, and Fedora. Each security advisory that waits is one finding, with an incident of its own. The check reads what dnf already knows, and downloads nothing.",
	UsedAs:                    UsedAsCheck,
	OneIncidentForEachFinding: true,
	Inputs: []ModuleInput{
		{Name: "fail_when", Type: "string", Required: true, AllowedValues: []any{"a_security_update_waits_to_be_installed"}, Description: "The check fails while a security update waits to be installed; write a_security_update_waits_to_be_installed, the only value."},
	},
	Examples: []ModuleExample{{
		Description:   "Tell the team when a security update waits to be installed.",
		Configuration: "dnf_security_updates:\n  fail_when: a_security_update_waits_to_be_installed\nalert_severity: warn\n",
	}},
}
