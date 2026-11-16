package primitives

import "strings"

// CompareDebianVersions compares valid Debian package versions using dpkg's
// epoch, upstream version, and Debian revision ordering. It returns -1, 0, or 1.
func CompareDebianVersions(a, b string) int {
	epochAndVersion := func(version string) (string, string) {
		if epoch, rest, found := strings.Cut(version, ":"); found {
			return epoch, rest
		}
		return "0", version
	}
	aEpoch, a := epochAndVersion(a)
	bEpoch, b := epochAndVersion(b)
	if order := compareDebianNumber(aEpoch, bEpoch); order != 0 {
		return order
	}
	upstreamAndRevision := func(version string) (string, string) {
		if last := strings.LastIndexByte(version, '-'); last >= 0 {
			return version[:last], version[last+1:]
		}
		return version, "0"
	}
	a, aRevision := upstreamAndRevision(a)
	b, bRevision := upstreamAndRevision(b)
	if order := compareDebianVersionPart(a, b); order != 0 {
		return order
	}
	return compareDebianVersionPart(aRevision, bRevision)
}

func compareDebianNumber(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) < len(b) {
		return -1
	}
	if len(a) > len(b) {
		return 1
	}
	return strings.Compare(a, b)
}

func debianVersionDigit(character byte) bool { return character >= '0' && character <= '9' }

// dpkg puts a tilde before even the end, letters before punctuation, and
// compares each run of digits as a number, without a machine integer limit.
func compareDebianVersionPart(a, b string) int {
	order := func(part string) int {
		if part == "" {
			return 0
		}
		character := part[0]
		switch {
		case character == '~':
			return -1
		case debianVersionDigit(character):
			return 0
		case character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z':
			return int(character)
		default:
			return int(character) + 256
		}
	}
	for a != "" || b != "" {
		for a != "" && !debianVersionDigit(a[0]) || b != "" && !debianVersionDigit(b[0]) {
			if difference := order(a) - order(b); difference < 0 {
				return -1
			} else if difference > 0 {
				return 1
			}
			if a != "" {
				a = a[1:]
			}
			if b != "" {
				b = b[1:]
			}
		}
		aEnd, bEnd := 0, 0
		for aEnd < len(a) && debianVersionDigit(a[aEnd]) {
			aEnd++
		}
		for bEnd < len(b) && debianVersionDigit(b[bEnd]) {
			bEnd++
		}
		if comparison := compareDebianNumber(a[:aEnd], b[:bEnd]); comparison != 0 {
			return comparison
		}
		a, b = a[aEnd:], b[bEnd:]
	}
	return 0
}
