package main

import (
	"bytes"
	"errors"
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
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
		"start", "stop", "serve", "service install", "service uninstall", "service status",
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

// TestParseServiceArgs covers `bb service` dispatch validation, which runs
// before any systemd or store access.
func TestParseServiceArgs(t *testing.T) {
	ok := []struct {
		args []string
		want serviceOpts
	}{
		{[]string{"install"}, serviceOpts{sub: "install"}},
		{[]string{"install", "--folder", "~/Work", "--delete", "--force"},
			serviceOpts{sub: "install", folder: "~/Work", deleteRemoved: true, force: true}},
		{[]string{"install", "--folder=/data"}, serviceOpts{sub: "install", folder: "/data"}},
		{[]string{"uninstall"}, serviceOpts{sub: "uninstall"}},
		{[]string{"status"}, serviceOpts{sub: "status"}},
	}
	for _, tc := range ok {
		got, err := parseServiceArgs(tc.args)
		if err != nil || got != tc.want {
			t.Errorf("parseServiceArgs(%q) = %+v, %v; want %+v", tc.args, got, err, tc.want)
		}
	}

	usageCases := [][]string{
		nil,                  // no subcommand
		{"bogus"},            // unknown subcommand
		{"install", "extra"}, // stray positional
		{"uninstall", "now"}, // extra arg
		{"status", "-v"},     // extra arg
	}
	for _, args := range usageCases {
		if _, err := parseServiceArgs(args); err == nil || !strings.Contains(err.Error(), "usage: bb service") {
			t.Errorf("parseServiceArgs(%q): want usage error, got %v", args, err)
		}
	}

	if _, err := parseServiceArgs([]string{"install", "--nope"}); !errors.Is(err, errFlagsReported) {
		t.Errorf("bad flag: want errFlagsReported, got %v", err)
	}
}

// TestVersionAndHelpCreateNoState is the regression for `bb version` creating
// ~/backup_repo (master key + empty DB) as a side effect: they now answer
// before config.Load.
func TestVersionAndHelpCreateNoState(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}, {"help"}, {"--help"}, {"-h"}} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home) // os.UserHomeDir on Windows
		if code := run(args); code != 0 {
			t.Errorf("run(%q) = %d, want 0", args, code)
		}
		if _, err := os.Stat(filepath.Join(home, "backup_repo")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("run(%q) created ~/backup_repo (stat err %v)", args, err)
		}
	}
}

// TestUnknownCommandCreatesNoState is the regression for `bb bogus` (any typo)
// creating ~/backup_repo before printing usage.
func TestUnknownCommandCreatesNoState(t *testing.T) {
	for _, args := range [][]string{{"bogus"}, {"staus"}, {"--nope"}} {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		if code := run(args); code != 1 {
			t.Errorf("run(%q) = %d, want 1", args, code)
		}
		if _, err := os.Stat(filepath.Join(home, "backup_repo")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("run(%q) created ~/backup_repo (stat err %v)", args, err)
		}
	}
}

// TestIsCommandMatchesDispatch parses run's dispatch switch and checks every
// case is accepted by isCommand, so adding a command without updating the
// allowlist fails here instead of silently rejecting the new command.
func TestIsCommandMatchesDispatch(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var cases []string
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "run" {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sw, ok := n.(*ast.SwitchStmt)
			if !ok {
				return true
			}
			if id, ok := sw.Tag.(*ast.Ident); !ok || id.Name != "cmd" {
				return true
			}
			for _, st := range sw.Body.List {
				for _, e := range st.(*ast.CaseClause).List {
					if lit, ok := e.(*ast.BasicLit); ok {
						cases = append(cases, strings.Trim(lit.Value, `"`))
					}
				}
			}
			return false
		})
		return false
	})
	if len(cases) < 15 {
		t.Fatalf("found only %d dispatch cases %q; did run's `switch cmd` change shape?", len(cases), cases)
	}
	for _, c := range cases {
		if !isCommand(c) {
			t.Errorf("run dispatches %q but isCommand rejects it", c)
		}
	}
	for _, bad := range []string{"bogus", "", "STATUS", "help", "version"} {
		if isCommand(bad) {
			t.Errorf("isCommand(%q) = true", bad)
		}
	}
}
