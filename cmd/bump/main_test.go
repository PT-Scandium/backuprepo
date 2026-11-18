package main

import (
	"os"
	"strings"
	"testing"
)

// writeVersion creates a VERSION file holding v in a fresh working directory.
func writeVersion(t *testing.T, v string) {
	t.Helper()
	t.Chdir(t.TempDir())
	if err := os.WriteFile(versionFile, []byte(v+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// readVersion returns the trimmed contents of VERSION in the working directory.
func readVersion(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(versionFile)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

// TestRunBumpsPastMajor20 is the end-to-end regression for the version bug:
// bumping 20.20.20 wrote 21.0.0, and the next bump then failed with
// `component "21" must be an integer 0..20`.
func TestRunBumpsPastMajor20(t *testing.T) {
	writeVersion(t, "20.20.20")
	for _, step := range []struct{ part, want string }{
		{"major", "21.0.0"},
		{"patch", "21.0.1"},
		{"minor", "21.1.0"},
	} {
		if err := run(step.part); err != nil {
			t.Fatalf("bump %s: %v", step.part, err)
		}
		if got := readVersion(t); got != step.want {
			t.Fatalf("after bump %s VERSION = %s, want %s", step.part, got, step.want)
		}
	}
}

// TestRunCarry verifies the default patch bump carries (1.0.20 -> 1.1.0).
func TestRunCarry(t *testing.T) {
	writeVersion(t, "1.0.20")
	if err := run("patch"); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := readVersion(t); got != "1.1.0" {
		t.Fatalf("VERSION = %s, want 1.1.0", got)
	}
}

// TestRunErrors verifies a bad VERSION, an unknown part, and a missing file
// are reported as errors and leave VERSION unchanged.
func TestRunErrors(t *testing.T) {
	writeVersion(t, "1.21.0")
	if err := run("patch"); err == nil {
		t.Fatal("run with out-of-range minor should error")
	}
	if got := readVersion(t); got != "1.21.0" {
		t.Fatalf("VERSION changed to %s after a failed bump", got)
	}

	writeVersion(t, "1.0.0")
	if err := run("build"); err == nil {
		t.Fatal("run with unknown part should error")
	}
	if got := readVersion(t); got != "1.0.0" {
		t.Fatalf("VERSION changed to %s after a failed bump", got)
	}

	t.Chdir(t.TempDir()) // no VERSION file
	if err := run("patch"); err == nil {
		t.Fatal("run without a VERSION file should error")
	}
}
