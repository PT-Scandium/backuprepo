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
		{"-- then flag-like args", []string{"--", "-x", "-r"}, false, false, "", []string{"-x", "-r"}},
		{"flags, positional, --", []string{"-r", "q", "--", "-f", "x"}, true, false, "", []string{"q", "-f", "x"}},
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

// usageLine returns the help line that starts with "  <cmd>" plus its indented
// continuation lines, so tests can check what is documented under a command.
func usageLine(t *testing.T, out, cmd string) string {
	t.Helper()
	lines := strings.Split(out, "\n")
	for i, l := range lines {
		if !strings.HasPrefix(l, "  "+cmd+" ") {
			continue
		}
		block := l
		for _, next := range lines[i+1:] {
			if !strings.HasPrefix(next, "                             ") { // continuation indent
				break
			}
			block += "\n" + next
		}
		return block
	}
	t.Fatalf("usage has no line for %q", cmd)
	return ""
}

// TestUsageAccuracy is the regression for help text that disagreed with the
// code: --delete was described under stop (it is a start flag), --backend was
// said to override upload (only ls/get/put/rm/find accept it), the version
// example was a stale literal, and the help/version aliases were missing.
func TestUsageAccuracy(t *testing.T) {
	var buf bytes.Buffer
	usage(&buf)
	out := buf.String()

	if start := usageLine(t, out, "start"); !strings.Contains(start, "--delete propagates") {
		t.Errorf("start's help should describe --delete:\n%s", start)
	}
	if stop := usageLine(t, out, "stop"); strings.Contains(stop, "--delete") {
		t.Errorf("stop takes no flags but its help mentions --delete:\n%s", stop)
	}
	for _, s := range []string{
		`"bb X.Y.Z" (also --version, -v)`,          // version output + aliases
		"  help ", "-h, --help", "or <command> -h", // help command + aliases
		"--backend s3|b2            override the stored backend for one ls / get / put / rm / find",
		"used by upload, start, serve and all file operations",
		"Override it for one ls / get / put / rm / find with  --backend s3|b2",
	} {
		if !strings.Contains(out, s) {
			t.Errorf("usage missing %q", s)
		}
	}
	for _, stale := range []string{"(e.g. 1.0.0)", "override the backend for one command", "applies to upload and all file operations"} {
		if strings.Contains(out, stale) {
			t.Errorf("usage still contains stale text %q", stale)
		}
	}
	// upload and start must not advertise --backend: their FlagSets reject it.
	for _, cmd := range []string{"upload", "start"} {
		if line := usageLine(t, out, cmd); strings.Contains(line, "--backend") {
			t.Errorf("%s's help mentions --backend, which it does not accept:\n%s", cmd, line)
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
		{[]string{"install", "--linger"}, serviceOpts{sub: "install", linger: true}},
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
	for _, args := range [][]string{{"version"}, {"--version"}, {"-v"}, {"help"}, {"--help"}, {"-h"}, {"--h"}, {"-help"}, {"-h=1"}} {
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

// TestHelpRequested covers the help-flag scan run does before touching state.
func TestHelpRequested(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want bool
	}{
		{[]string{"-h"}, true},
		{[]string{"--h"}, true},
		{[]string{"-help"}, true},
		{[]string{"--help"}, true},
		{[]string{"photos/", "-r", "-h"}, true}, // after positionals, like parseFlags
		{[]string{"install", "--help"}, true},   // bb service install --help
		{[]string{"--", "-h"}, false},           // after "--" every argument is literal
		{[]string{"-r", "--", "--help"}, false},
		{[]string{"--", "foo", "-h"}, false},
		{[]string{"-h=1"}, true}, // the flag package treats -h=x as help too
		{[]string{"x", "--help=true"}, true},
		{[]string{"-hh"}, false},
		{[]string{"-hh=1"}, false},
		{[]string{"help"}, false}, // bare word: a folder or query named "help"
		{[]string{"photos/", "-r"}, false},
		{nil, false},
	} {
		if got := helpRequested(tc.args); got != tc.want {
			t.Errorf("helpRequested(%q) = %v, want %v", tc.args, got, tc.want)
		}
	}
}

// TestSubcommandHelpCreatesNoState is the regression for `bb <cmd> -h`: flag
// commands exited 1 after creating ~/backup_repo, no-flag commands (init, stop,
// list) ignored -h and ran, and positional commands took "-h" as a folder,
// bucket or key name. Every dispatched command must now print help, exit 0 and
// leave no state behind. Exit 0 alone proves little (list/status already
// exit 0), so the no-state check is what catches a command that ran.
func TestSubcommandHelpCreatesNoState(t *testing.T) {
	var cases [][]string
	for _, c := range dispatchCommands(t) {
		cases = append(cases, []string{c, "-h"}, []string{c, "--help"}, []string{c, "x", "-help"}, []string{c, "--h=1"})
	}
	cases = append(cases, []string{"service", "install", "-h"}, []string{"service", "install", "--folder", "/tmp", "--h"})
	for _, args := range cases {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		if code := run(args); code != 0 {
			t.Errorf("run(%q) = %d, want 0", args, code)
		}
		if _, err := os.Stat(filepath.Join(home, "backup_repo")); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("run(%q) created ~/backup_repo (stat err %v)", args, err)
		}
	}
}

// dispatchCommands parses main.go and returns the string cases of run's
// `switch cmd` — the commands bb dispatches after opening the store.
func dispatchCommands(t *testing.T) []string {
	t.Helper()
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
	return cases
}

// TestUsageErrorsCreateNoState is the regression for argument and flag errors
// creating ~/backup_repo before failing: run used to load config and open the
// store before checking the command line.
func TestUsageErrorsCreateNoState(t *testing.T) {
	for _, args := range [][]string{
		{"watch"}, {"watch", "a", "b"}, {"unwatch"}, {"find"}, {"get"}, {"put"}, {"rm"},
		{"bucket", "a", "b", "c"}, {"appkey", "a", "b"}, {"backend", "s3", "b2"},
		{"ls", "--nope"}, {"upload", "--nope"}, {"start", "--nope"}, {"rm", "-q", "x"},
		{"service"}, {"service", "bogus"}, {"service", "install", "--nope"}, {"service", "status", "x"},
		{"ls", "--backend", "bogus"}, {"find", "--backend", "x", "q"}, {"rm", "p", "--backend=s4"}, {"backend", "bogus"},
	} {
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
	for _, c := range dispatchCommands(t) {
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

// TestBucketIDLookupWiring guards the bucket-scoped-key fix at its user-facing
// wiring: init and bucket must resolve the bucket ID with b2.FindBucket (which
// names the bucket in b2_list_buckets, as B2 requires for restricted keys), not
// the unfiltered b2.ListBuckets. The cli tests stub the lister, so only this
// test notices a regression in main.go.
func TestBucketIDLookupWiring(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		fn, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || fn.Sel.Name != "Init" && fn.Sel.Name != "Bucket" {
			return true
		}
		if pkg, ok := fn.X.(*ast.Ident); !ok || pkg.Name != "cli" {
			return true
		}
		for _, arg := range call.Args {
			sel, ok := arg.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "b2" {
				if sel.Sel.Name != "FindBucket" {
					t.Errorf("cli.%s is passed b2.%s; want b2.FindBucket", fn.Sel.Name, sel.Sel.Name)
				}
				found[fn.Sel.Name] = true
			}
		}
		return true
	})
	for _, name := range []string{"Init", "Bucket"} {
		if !found[name] {
			t.Errorf("no cli.%s call with a b2 bucket lookup found in main.go", name)
		}
	}
	// cli.Buckets takes (listAll, findOne): both have the same type, so only
	// this check notices them swapped (the scoped-key fallback would then
	// list via FindBucket and "fall back" to the full list).
	var bucketsArgs []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if fn, ok := call.Fun.(*ast.SelectorExpr); ok && fn.Sel.Name == "Buckets" {
			if pkg, ok := fn.X.(*ast.Ident); ok && pkg.Name == "cli" {
				for _, arg := range call.Args {
					if sel, ok := arg.(*ast.SelectorExpr); ok {
						if p, ok := sel.X.(*ast.Ident); ok && p.Name == "b2" {
							bucketsArgs = append(bucketsArgs, sel.Sel.Name)
						}
					}
				}
			}
		}
		return true
	})
	if strings.Join(bucketsArgs, ",") != "ListBuckets,FindBucket" {
		t.Errorf("cli.Buckets is passed b2.%v; want ListBuckets then FindBucket", bucketsArgs)
	}
}
