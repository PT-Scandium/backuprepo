package main

import (
	"bytes"
	"flag"
	"reflect"
	"strings"
	"testing"
)

// TestParseFlagsAnyOrder verifies flags are honored whether they appear before,
// after, or interspersed with positional args (stdlib flag.Parse stops at the
// first positional; parseFlags resumes past each one).
func TestParseFlagsAnyOrder(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantR   bool
		wantF   bool
		wantBE  string
		wantPos []string
	}{
		{"flags first", []string{"-r", "-f", "brtest/"}, true, true, "", []string{"brtest/"}},
		{"flags after path", []string{"brtest/", "-r", "-f"}, true, true, "", []string{"brtest/"}},
		{"interspersed", []string{"-r", "brtest/", "-f"}, true, true, "", []string{"brtest/"}},
		{"value flag after path", []string{"photos/", "--backend", "b2"}, false, false, "b2", []string{"photos/"}},
		{"two positionals + trailing flag", []string{"a", "b", "-r"}, true, false, "", []string{"a", "b"}},
		{"no flags", []string{"only/"}, false, false, "", []string{"only/"}},
		{"no args", nil, false, false, "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fs := flag.NewFlagSet("t", flag.ContinueOnError)
			r := fs.Bool("r", false, "")
			f := fs.Bool("f", false, "")
			be := fs.String("backend", "", "")
			pos, err := parseFlags(fs, tc.args)
			if err != nil {
				t.Fatalf("parseFlags: %v", err)
			}
			if *r != tc.wantR || *f != tc.wantF || *be != tc.wantBE {
				t.Errorf("flags = r:%v f:%v backend:%q, want r:%v f:%v backend:%q",
					*r, *f, *be, tc.wantR, tc.wantF, tc.wantBE)
			}
			if !reflect.DeepEqual(pos, tc.wantPos) {
				t.Errorf("positionals = %#v, want %#v", pos, tc.wantPos)
			}
		})
	}
}

// TestUsageCoversAllCommandsAndModes verifies the help output documents every
// command and shows both backend modes with worked examples.
func TestUsageCoversAllCommandsAndModes(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	out := buf.String()

	commands := []string{
		"init", "config", "watch", "unwatch", "list", "status", "upload",
		"backend", "ls", "get", "put", "rm", "find",
	}
	for _, c := range commands {
		if !strings.Contains(out, c) {
			t.Errorf("usage output missing command %q", c)
		}
	}

	mustContain := []string{
		"--backend",          // per-command override flag documented
		"backend b2",         // switching to native B2 mode
		"bb ls --backend s3", // one-off override example
		"-r",                 // recursive flag
		"-f, -y",             // rm confirmation skip
		"S3-compatible",      // both modes described
		"native Backblaze B2",
		"EXAMPLES",
		"SETUP",
		"MANUAL FILE OPERATIONS",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("usage output missing %q", s)
		}
	}
}
