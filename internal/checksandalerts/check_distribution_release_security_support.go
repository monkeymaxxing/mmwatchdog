package checksandalerts

import (
	"context"
	"encoding/csv"
	"encoding/xml"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"mmwatchdog/internal/primitives"
)

// DistributionReleaseSecuritySupport is the inputs of the
// distribution_release_security_support check module, which reads the
// release dates of distro-info-data on Debian and Ubuntu, the product file
// of the release on SUSE, and the SUPPORT_END of /etc/os-release on the Red
// Hat family.
type DistributionReleaseSecuritySupport struct {
	SecuritySupport                   string   `yaml:"security_support" json:"security_support"`
	FailWhenSecuritySupportEndsWithin Duration `yaml:"fail_when_security_support_ends_within" json:"fail_when_security_support_ends_within"`
}

func (n *DistributionReleaseSecuritySupport) Validate(v InputValidation) *FieldError {
	inputs := map[string]any{}
	if n.SecuritySupport != "" || v.Authored != nil && v.Authored("security_support") {
		inputs["security_support"] = n.SecuritySupport
	}
	if n.FailWhenSecuritySupportEndsWithin != "" || v.Authored != nil && v.Authored("fail_when_security_support_ends_within") {
		inputs["fail_when_security_support_ends_within"] = string(n.FailWhenSecuritySupportEndsWithin)
	}
	return distributionReleaseSecuritySupportDescription.InputsProblem("distribution_release_security_support", inputs)
}

func (n *DistributionReleaseSecuritySupport) Run(_ context.Context, _ *CheckRun) Observation {
	release := primitives.NodeOSRelease()
	family, prettyName := primitives.NodeDistributionFamily()
	name := namedOrUnnamed(prettyName)
	id := release["ID"]
	if family == primitives.SUSEFamily {
		return n.runOnSUSE(name)
	}
	if family == primitives.RedHatFamily {
		return n.runOnRedHat(release, name)
	}
	if id != "debian" && id != "ubuntu" {
		return Result("unknown", "MMWatchdog does not know the security support of "+name+": it knows only that of Debian, Ubuntu, the Red Hat family, and SUSE")
	}
	support, applies := distributionReleaseSecuritySupports[id][n.SecuritySupport]
	if !applies {
		other := map[string]string{"debian": "Ubuntu", "ubuntu": "Debian"}[id]
		return Result("unknown", "security_support: "+n.SecuritySupport+" applies only to "+other+", and this node runs "+name)
	}
	codename := release["VERSION_CODENAME"]
	if codename == "" {
		return Result("unknown", "/etc/os-release names no VERSION_CODENAME, so MMWatchdog cannot find the release of this node")
	}
	path := "/usr/share/distro-info/" + id + ".csv"
	data, err := primitives.ReadFile(primitives.NodeFile(path), primitives.ProcFileLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return Result("unknown", "MMWatchdog cannot read "+path+", which holds the dates of the releases: install the package distro-info-data")
	}
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read "+path+": "+err.Error())
	}
	reader := csv.NewReader(strings.NewReader(string(data)))
	reader.FieldsPerRecord = -1 // Releases without dates have shorter rows.
	rows, err := reader.ReadAll()
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read "+path+": "+err.Error())
	}
	columns := map[string]int{}
	if len(rows) > 0 {
		for index, column := range rows[0] {
			columns[column] = index
		}
	}
	cell := func(row []string, column string) string {
		if index, found := columns[column]; found && index < len(row) {
			return row[index]
		}
		return ""
	}
	deadline, date := support.name, ""
	for index, row := range rows {
		if index > 0 && cell(row, "series") == codename {
			date = cell(row, support.column)
			if date == "" {
				date = cell(row, "eol")
				deadline = "the release has no end date of " + support.name + ", and the release's support"
			}
			break
		}
	}
	if date == "" {
		return Result("unknown", path+" gives no end of "+support.name+" for "+codename+": update the package distro-info-data")
	}
	end, err := time.Parse("2006-01-02", date)
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read "+path+": "+err.Error())
	}
	return n.observationOfEnd(name, deadline, date, end)
}

// observationOfEnd says whether the end of a security support is near
// enough to fail, and how far away it is.
func (n *DistributionReleaseSecuritySupport) observationOfEnd(name, deadline, date string, end time.Time) Observation {
	left := time.Until(end)
	status := "ok"
	if left <= n.FailWhenSecuritySupportEndsWithin.Value() {
		status = "failed"
	}
	days := int(left.Abs() / (24 * time.Hour))
	span := countOf(days, "day", "days")
	if days >= 60 {
		span = fmt.Sprintf("%d months", int(float64(days)/30.4375))
	}
	if left < 0 {
		return Result(status, "this node runs "+name+": "+deadline+" ended on "+date+", "+span+" ago")
	}
	return Result(status, "this node runs "+name+": "+deadline+" ends on "+date+", in "+span)
}

// distributionReleaseSecuritySupports keeps each deadline column with the name
// that the check reports for that deadline.
var distributionReleaseSecuritySupports = map[string]map[string]struct{ column, name string }{
	"debian": {
		"regular":    {"eol", "Debian's regular security support"},
		"debian_lts": {"eol-lts", "Debian LTS"},
	},
	"ubuntu": {
		"regular":    {"eol-server", "Ubuntu's standard security maintenance"},
		"ubuntu_pro": {"eol-esm", "Ubuntu Pro's Expanded Security Maintenance"},
	},
}

// distributionReleaseSecuritySupportDescription is what "mmwatchdog describe-module
// distribution_release_security_support" shows, and what validation knows about the inputs.
var distributionReleaseSecuritySupportDescription = ModuleDescription{
	Summary: "Fails when the security support that the check names ends soon for the node's Debian, Ubuntu, Red Hat family, or SUSE release, or has ended. The check reads the release dates of the package distro-info-data on Debian and Ubuntu, the SUPPORT_END of /etc/os-release on the Red Hat family, and the product file of the release on SUSE.",
	UsedAs:  UsedAsCheck,
	Inputs: []ModuleInput{
		{Name: "security_support", Type: "string", Required: true, AllowedValues: []any{"regular", "debian_lts", "ubuntu_pro"}, Description: "The security support that the check counts down to. regular: Debian's regular security support (the column eol) or Ubuntu's standard security maintenance (eol-server). debian_lts: Debian LTS (eol-lts), on Debian only. ubuntu_pro: Ubuntu Pro's Expanded Security Maintenance (eol-esm), on Ubuntu only. When the release has no date in the chosen column, the check uses the column eol. On SUSE, such as openSUSE Leap, regular is the end of the release (the endoflife of /etc/products.d/baseproduct), and on the Red Hat family, such as Rocky Linux and AlmaLinux, it is the SUPPORT_END of /etc/os-release. On both, the other two values make the check unknown."},
		{Name: "fail_when_security_support_ends_within", Type: "duration", Required: true, Description: "The check fails when the security support that security_support chooses ends within this time for the node's release, such as 180d."},
	},
	Examples: []ModuleExample{{
		Description:   "Tell the team before the release's regular security support ends.",
		Configuration: "distribution_release_security_support:\n  security_support: regular\n  fail_when_security_support_ends_within: 180d\nalert_severity: warn\n",
	}},
}

// suseProduct is the part of the product file of SUSE that the check reads.
// endoflife is read only as a child of product: the codestream element
// holds another endoflife, the end of the whole Leap 16 line.
type suseProduct struct {
	EndOfLife []string `xml:"endoflife"`
}

// runOnSUSE reads the end of the release's security support from
// /etc/products.d/baseproduct, a link to the product file that the release
// package installs. SUSE gives one end date, the regular one.
func (n *DistributionReleaseSecuritySupport) runOnSUSE(name string) Observation {
	if o := n.otherSupportThanRegular(name); o != nil {
		return *o
	}
	const path = "/etc/products.d/baseproduct"
	data, err := primitives.ReadFile(primitives.NodeFile(path), primitives.ProcFileLimit)
	if errors.Is(err, fs.ErrNotExist) {
		return Result("unknown", "MMWatchdog cannot read "+path+", the product file of this release, which the release package of "+name+" installs")
	}
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read "+path+": "+err.Error())
	}
	var product suseProduct
	if err := xml.Unmarshal(data, &product); err != nil {
		return Result("unknown", "MMWatchdog cannot read "+path+": "+err.Error())
	}
	if len(product.EndOfLife) == 0 || strings.TrimSpace(product.EndOfLife[0]) == "" {
		return Result("unknown", path+" gives no endoflife for this release")
	}
	date := strings.TrimSpace(product.EndOfLife[0])
	end, err := time.Parse("2006-01-02", date)
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read the endoflife of "+path+": "+err.Error())
	}
	return n.observationOfEnd(name, "SUSE's regular security support", date, end)
}

// otherSupportThanRegular is the unknown observation for a distribution that
// states one end of security support, the regular one, when the check asks
// for the support of another distribution; nil when the check asks for the
// regular one.
func (n *DistributionReleaseSecuritySupport) otherSupportThanRegular(name string) *Observation {
	if n.SecuritySupport == "regular" {
		return nil
	}
	other := map[string]string{"debian_lts": "Debian", "ubuntu_pro": "Ubuntu"}[n.SecuritySupport]
	o := Result("unknown", "security_support: "+n.SecuritySupport+" applies only to "+other+", and this node runs "+name)
	return &o
}

// runOnRedHat reads SUPPORT_END from /etc/os-release, a date in the form
// 2032-05-31. The release package of the distribution installs the file,
// and the vendor states in it the end of the release's support: Rocky Linux
// and AlmaLinux give the end of their releases' updates. They give one end
// date, the regular one.
func (n *DistributionReleaseSecuritySupport) runOnRedHat(release map[string]string, name string) Observation {
	if o := n.otherSupportThanRegular(name); o != nil {
		return *o
	}
	date := release["SUPPORT_END"]
	if date == "" {
		return Result("unknown", "/etc/os-release gives no SUPPORT_END for this release, which the release package of "+name+" installs")
	}
	end, err := time.Parse("2006-01-02", date)
	if err != nil {
		return Result("unknown", "MMWatchdog cannot read the SUPPORT_END of /etc/os-release: "+err.Error())
	}
	return n.observationOfEnd(name, "the regular security support that the release states", date, end)
}
