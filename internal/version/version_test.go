package version

import "testing"

// TestParseValid checks that well-formed versions parse to the right components.
func TestParseValid(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want V
	}{
		{"0.0.0", V{0, 0, 0}},
		{"1.0.0", V{1, 0, 0}},
		{"1.20.20", V{1, 20, 20}},
		{" 3.4.5 \n", V{3, 4, 5}}, // surrounding whitespace tolerated
	} {
		got, err := Parse(tc.in)
		if err != nil || got != tc.want {
			t.Fatalf("Parse(%q) = %v, %v; want %v", tc.in, got, err, tc.want)
		}
	}
}

// TestParseInvalid checks malformed versions and out-of-range components are rejected.
func TestParseInvalid(t *testing.T) {
	for _, in := range []string{"", "1", "1.2", "1.2.3.4", "1.2.x", "1.2.21", "-1.0.0", "1.0.99"} {
		if _, err := Parse(in); err == nil {
			t.Fatalf("Parse(%q) should have errored", in)
		}
	}
}

// TestBumpPatchCarry verifies the odometer carry: patch wraps at Max into minor,
// and minor wraps into major.
func TestBumpPatchCarry(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1.0.0", "1.0.1"},
		{"1.0.19", "1.0.20"},
		{"1.0.20", "1.1.0"},  // the example from the spec
		{"1.20.20", "2.0.0"}, // double carry
		{"0.0.20", "0.1.0"},
		{"5.20.20", "6.0.0"},
	} {
		v, _ := Parse(tc.in)
		got, err := v.Bump("patch")
		if err != nil || got.String() != tc.want {
			t.Fatalf("%s bump patch = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	// "" defaults to patch.
	v, _ := Parse("1.2.3")
	if got, _ := v.Bump(""); got.String() != "1.2.4" {
		t.Fatalf(`bump "" = %s; want 1.2.4`, got)
	}
}

// TestBumpMinor verifies minor bumps reset patch and carry into major.
func TestBumpMinor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1.0.5", "1.1.0"},
		{"1.20.7", "2.0.0"}, // minor at Max carries into major
		{"0.0.0", "0.1.0"},
	} {
		v, _ := Parse(tc.in)
		if got, _ := v.Bump("minor"); got.String() != tc.want {
			t.Fatalf("%s bump minor = %s; want %s", tc.in, got, tc.want)
		}
	}
}

// TestBumpMajor verifies major bumps reset minor and patch and never wrap.
func TestBumpMajor(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"1.2.3", "2.0.0"},
		{"20.20.20", "21.0.0"}, // major is unbounded
	} {
		v, _ := Parse(tc.in)
		if got, _ := v.Bump("major"); got.String() != tc.want {
			t.Fatalf("%s bump major = %s; want %s", tc.in, got, tc.want)
		}
	}
}

// TestBumpUnknownPart rejects parts other than major/minor/patch.
func TestBumpUnknownPart(t *testing.T) {
	v, _ := Parse("1.0.0")
	if _, err := v.Bump("build"); err == nil {
		t.Fatal("Bump with unknown part should error")
	}
}
