package primitives_test

import (
	"testing"

	"mmwatchdog/internal/primitives"
)

func TestCompareDebianVersions(t *testing.T) {
	for _, test := range []struct {
		a, b string
		want int
	}{
		{"1.0~rc1", "1.0", -1},
		{"1:0.1", "2.0", 1},
		{"1.0-1", "1.0-1+deb13u1", -1},
		{"1.9.16p2-2", "1.9.16p2-3", -1},
		{"6.8.0-147.147", "6.8.0-45.45", 1},
		{"2.30-1", "2.30-1.1", -1},
		{"1.0a", "1.0+", -1},
		{"1.0", "1.0", 0},
		{"1.0", "0:1.0-0", 0},
		{"1.01", "1.1", 0},
		{"1.0~~", "1.0~", -1},
		{"1.0~", "1.0", -1},
		{"1.0+", "1.0.", -1},
		{"1.0-1-2", "1.0-1-3", -1},
		{"10:1", "2:1", 1},
		{"1.99999999999999999999", "1.2", 1},
	} {
		t.Run(test.a+" vs "+test.b, func(t *testing.T) {
			if got := primitives.CompareDebianVersions(test.a, test.b); got != test.want {
				t.Fatalf("got %d, want %d", got, test.want)
			}
			if got := primitives.CompareDebianVersions(test.b, test.a); got != -test.want {
				t.Fatalf("reverse: got %d, want %d", got, -test.want)
			}
		})
	}
}
