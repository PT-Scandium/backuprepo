// Package version implements backuprepo's odometer-style version scheme.
//
// A version is a major.minor.patch tuple where minor and patch run 0..Max
// (i.e. 21 distinct values). Incrementing one past Max resets it to 0 and
// carries into the next-higher component — e.g. 1.0.20 bumps to 1.1.0 and
// 1.20.20 bumps to 2.0.0. Major is the top component and is unbounded (it never
// wraps, since there is nothing to carry into).
package version

import (
	"fmt"
	"strconv"
	"strings"
)

// Max is the highest value minor or patch may hold; the next increment wraps it
// to 0 and carries. So each cycles through 21 values, 0..20. Major has no limit.
const Max = 20

// V is a major.minor.patch version.
type V struct {
	Major, Minor, Patch int
}

// Parse reads a "major.minor.patch" string. Minor and patch must be integers in
// 0..Max; major is any non-negative integer, since Bump carries into it without
// bound (so a VERSION that Bump wrote, e.g. 21.0.0, always parses back).
func Parse(s string) (V, error) {
	s = strings.TrimSpace(s) // VERSION files end in a newline; keep it out of errors
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return V{}, fmt.Errorf("version %q: want major.minor.patch", s)
	}
	n := make([]int, 3)
	for i, p := range parts {
		val, err := strconv.Atoi(p)
		if err != nil || val < 0 {
			return V{}, fmt.Errorf("version %q: component %q must be a non-negative integer", s, p)
		}
		if i > 0 && val > Max {
			return V{}, fmt.Errorf("version %q: minor/patch %q must be 0..%d", s, p, Max)
		}
		n[i] = val
	}
	return V{Major: n[0], Minor: n[1], Patch: n[2]}, nil
}

// String formats the version as "major.minor.patch".
func (v V) String() string { return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch) }

// Bump returns the next version. part selects the component to advance:
//   - "patch" (or ""): increment patch, carrying into minor and then major when
//     it passes Max (the odometer step, e.g. 1.0.20 -> 1.1.0).
//   - "minor": reset patch to 0 and increment minor, carrying into major.
//   - "major": reset minor and patch to 0 and increment major (unbounded).
//
// An unknown part returns an error.
func (v V) Bump(part string) (V, error) {
	switch part {
	case "", "patch":
		v.Patch++
		if v.Patch > Max {
			v.Patch = 0
			v.carryMinor()
		}
	case "minor":
		v.Patch = 0
		v.carryMinor()
	case "major":
		v.Patch = 0
		v.Minor = 0
		v.Major++
	default:
		return V{}, fmt.Errorf("unknown version part %q (want major, minor, or patch)", part)
	}
	return v, nil
}

// carryMinor increments minor, carrying into major when it passes Max.
func (v *V) carryMinor() {
	v.Minor++
	if v.Minor > Max {
		v.Minor = 0
		v.Major++
	}
}
