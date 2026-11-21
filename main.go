// Command backuprepo (installed as bb) backs up watched folders to a Backblaze
// B2 bucket over the S3-compatible API or the native B2 API. It dispatches the
// CLI subcommands, including the background daemon (start/stop), the localhost
// web UI (serve) and the systemd user service (service).
package main

import (
	"context"
	_ "embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"time"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/cli"
	"backuprepo/internal/config"
	"backuprepo/internal/service"
	"backuprepo/internal/store"
)

// versionFile is the build version, embedded from the VERSION file at compile
// time so `bb version` reports exactly what was built. See internal/version for
// the numbering scheme.
//
//go:embed VERSION
var versionFile string

// main runs the CLI and exits with its status code.
func main() {
	os.Exit(run(os.Args[1:]))
}

// run answers version/help requests and rejects unknown commands without
// touching state, then loads config, opens the store, dispatches the
// subcommand, and returns an exit code (0 success, 1 error).
func run(args []string) int {
	// version/help need no state: answer them before config.Load / store.Open,
	// which create ~/backup_repo, its master key and an empty database.
	if len(args) > 0 {
		switch args[0] {
		case "version", "--version", "-v":
			fmt.Fprintf(os.Stdout, "bb %s\n", strings.TrimSpace(versionFile))
			return 0
		}
		if args[0] == "help" || isHelpArg(args[0]) {
			usage(os.Stdout)
			return 0
		}
		// Reject typos before config.Load / store.Open create ~/backup_repo.
		if !isCommand(args[0]) {
			usage(os.Stderr)
			return 1
		}
		// `bb <cmd> -h` asks for help, so answer it before any state exists and
		// before a command like init, stop or watch could act on it.
		if helpRequested(args[1:]) {
			usage(os.Stdout)
			return 0
		}
	}

	cmd := "status"
	var rest []string
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}

	// Phase 1 checks the arguments and flags and picks the action without
	// touching state, so a usage or flag error never creates ~/backup_repo.
	// The actions capture cfg and st, which phase 2 fills in before running one.
	ctx := context.Background()
	var (
		cfg *config.Config
		st  *store.Store
		act func() error
	)
	switch cmd {
	case "init":
		act = func() error { return cli.Init(ctx, st, os.Stdin, os.Stdout, b2.FindBucket) }
	case "watch":
		fs := flag.NewFlagSet("watch", flag.ContinueOnError)
		remote := fs.String("remote", "", "store this folder's files under this bucket prefix")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		if len(pos) != 1 {
			return fail(fmt.Errorf("usage: bb watch /path/to/dir [--remote PREFIX]"))
		}
		if flagSet(fs, "remote") {
			act = func() error { return cli.WatchRemote(ctx, st, pos[0], *remote, os.Stdout) }
		} else {
			act = func() error { return cli.Watch(ctx, st, pos[0], os.Stdout) }
		}
	case "unwatch":
		if len(rest) != 1 {
			return fail(fmt.Errorf("usage: bb unwatch /path/to/dir"))
		}
		act = func() error { return cli.Unwatch(ctx, st, rest[0], os.Stdout) }
	case "list":
		act = func() error { return cli.List(ctx, st, os.Stdout) }
	case "status":
		act = func() error { return cli.Status(ctx, st, cfg.Dir, serviceProbe(), os.Stdout) }
	case "config":
		act = func() error { return cli.Config(ctx, st, os.Stdout) }
	case "upload":
		fs := flag.NewFlagSet("upload", flag.ContinueOnError)
		del := fs.Bool("delete", false, "also remove remote objects whose local files were deleted")
		if _, e := parseFlags(fs, rest); e != nil {
			return 1
		}
		act = func() error { return runUpload(ctx, st, *del) }
	case "ls":
		fs := flag.NewFlagSet("ls", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		if !validBackend(*backend) {
			return fail(apperr.ErrInvalidBackend)
		}
		act = func() error {
			be, err := buildBackend(ctx, st, *backend)
			if err != nil {
				return err
			}
			return cli.Ls(ctx, be, argAt(pos, 0), *recursive, os.Stdout)
		}
	case "find":
		fs := flag.NewFlagSet("find", flag.ContinueOnError)
		backend := fs.String("backend", "", "override backend (s3|b2)")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		if !validBackend(*backend) {
			return fail(apperr.ErrInvalidBackend)
		}
		if len(pos) < 1 {
			return fail(fmt.Errorf("usage: bb find <query> [prefix]"))
		}
		act = func() error {
			be, err := buildBackend(ctx, st, *backend)
			if err != nil {
				return err
			}
			return cli.Find(ctx, be, argAt(pos, 0), argAt(pos, 1), os.Stdout)
		}
	case "get":
		fs := flag.NewFlagSet("get", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		if !validBackend(*backend) {
			return fail(apperr.ErrInvalidBackend)
		}
		if len(pos) < 1 {
			return fail(fmt.Errorf("usage: bb get <remote> [local] [-r]"))
		}
		act = func() error {
			be, err := buildBackend(ctx, st, *backend)
			if err != nil {
				return err
			}
			return cli.Get(ctx, be, argAt(pos, 0), argAt(pos, 1), *recursive, os.Stdout)
		}
	case "put":
		fs := flag.NewFlagSet("put", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		skipExisting := fs.Bool("skip-existing", false, "skip files already present remotely (resume an interrupted upload)")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		if !validBackend(*backend) {
			return fail(apperr.ErrInvalidBackend)
		}
		if len(pos) < 1 {
			return fail(fmt.Errorf("usage: bb put <local> [remote] [-r] [--skip-existing]"))
		}
		act = func() error {
			be, err := buildBackend(ctx, st, *backend)
			if err != nil {
				return err
			}
			return cli.Put(ctx, be, argAt(pos, 0), argAt(pos, 1), *recursive, *skipExisting, os.Stdout)
		}
	case "rm":
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		force := fs.Bool("f", false, "skip confirmation")
		fs.BoolVar(force, "y", false, "skip confirmation")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		if !validBackend(*backend) {
			return fail(apperr.ErrInvalidBackend)
		}
		if len(pos) < 1 {
			return fail(fmt.Errorf("usage: bb rm <path> [-r] [-f]"))
		}
		act = func() error {
			be, err := buildBackend(ctx, st, *backend)
			if err != nil {
				return err
			}
			rc, err := st.GetConfig(ctx)
			if err != nil {
				return err
			}
			return cli.Rm(ctx, be, rc.Bucket, argAt(pos, 0), *recursive, *force, os.Stdin, os.Stdout)
		}
	case "backend":
		if len(rest) > 1 {
			return fail(fmt.Errorf("usage: bb backend [s3|b2]"))
		}
		if !validBackend(argAt(rest, 0)) {
			return fail(apperr.ErrInvalidBackend)
		}
		act = func() error { return cli.Backend(ctx, st, argAt(rest, 0), os.Stdout) }
	case "bucket":
		if len(rest) > 2 {
			return fail(fmt.Errorf("usage: bb bucket [<name> [<bucket-id>]]"))
		}
		act = func() error { return cli.Bucket(ctx, st, argAt(rest, 0), argAt(rest, 1), b2.FindBucket, os.Stdout) }
	case "buckets":
		act = func() error {
			bcfg, err := b2ConfigFromStore(ctx, st)
			if err != nil {
				return err
			}
			return cli.Buckets(ctx, bcfg, b2.ListBuckets, b2.FindBucket, os.Stdout)
		}
	case "appkey":
		if len(rest) > 1 {
			return fail(fmt.Errorf("usage: bb appkey [<new-keyID>]  (secret read from stdin)"))
		}
		act = func() error { return cli.SetAppKey(ctx, st, argAt(rest, 0), os.Stdin, os.Stdout) }
	case "start":
		fs := flag.NewFlagSet("start", flag.ContinueOnError)
		del := fs.Bool("delete", false, "also remove remote objects whose local files were deleted")
		if _, e := parseFlags(fs, rest); e != nil {
			return 1
		}
		act = func() error { return runStart(ctx, st, cfg, *del) }
	case "stop":
		act = func() error { return cli.Stop(ctx, cfg.Dir, os.Stdout) }
	case "serve":
		act = func() error { return runServe(ctx, st) }
	case "service":
		opts, e := parseServiceArgs(rest)
		if errors.Is(e, errFlagsReported) {
			return 1
		}
		if e != nil {
			return fail(e)
		}
		act = func() error { return runService(ctx, st, cfg, opts) }
	default:
		usage(os.Stderr)
		return 1
	}

	// Phase 2: load config and open the store (creating ~/backup_repo, its
	// master key and an empty database on first use), then run the action.
	var err error
	if cfg, err = config.Load(ctx); err != nil {
		return fail(err)
	}
	if st, err = store.Open(ctx, cfg.DBPath, cfg.Key); err != nil {
		return fail(err)
	}
	defer st.Close()
	if err := act(); err != nil {
		return fail(err)
	}
	return 0
}

// isCommand reports whether name is a subcommand run dispatches after opening
// the store (version and help are answered earlier). Anything else is rejected
// before any state is created. TestIsCommandMatchesDispatch parses run's
// switch to keep this list in sync.
func isCommand(name string) bool {
	switch name {
	case "init", "watch", "unwatch", "list", "status", "config", "upload",
		"ls", "find", "get", "put", "rm", "backend", "bucket", "buckets",
		"appkey", "start", "stop", "serve", "service":
		return true
	}
	return false
}

// helpRequested reports whether args (a subcommand's arguments) ask for help
// (see isHelpArg). It checks every argument, not just the first, so
// `bb ls photos/ -h` counts. It stops at "--", so `bb find -- -h` still
// searches for "-h" (parseFlags treats everything after "--" as positional).
// Commands that take only positionals (watch, bucket, appkey, backend, ...)
// would otherwise treat "-h" as a path, bucket or key name, and init/stop/list
// would ignore it and run. A flag value of "-h" (`--folder -h`) also counts as
// help.
func helpRequested(args []string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		if isHelpArg(a) {
			return true
		}
	}
	return false
}

// isHelpArg reports whether a is a spelling the flag package treats as a help
// request: -h, --h, -help or --help, with or without an "=value" suffix.
func isHelpArg(a string) bool {
	name, _, _ := strings.Cut(a, "=")
	switch name {
	case "-h", "--h", "-help", "--help":
		return true
	}
	return false
}

// parseFlags parses args while allowing flags and positional arguments to
// appear in any order, returning the positionals. The stdlib flag parser stops
// at the first non-flag token, so we resume parsing after each positional —
// this lets `ls photos/ -r` and `rm brtest/ -r -f` behave like `ls -r photos/`.
// A "--" ends flag parsing for good: everything after it is positional, so
// `bb find -- -x -h` searches for "-x" with prefix "-h". (A string flag given
// "--" as a separate value, `--backend -- x`, also reads as the terminator;
// harmless, since such a value fails validation anyway.)
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
		if consumed := len(args) - len(rest); consumed > 0 && args[consumed-1] == "--" {
			return append(positionals, rest...), nil
		}
		if len(rest) == 0 {
			return positionals, nil
		}
		positionals = append(positionals, rest[0])
		args = rest[1:]
	}
}

// argAt returns the i-th positional, or "" if absent.
func argAt(pos []string, i int) string {
	if i < len(pos) {
		return pos[i]
	}
	return ""
}

// runUpload builds the effective backend and uploads all changed files.
func runUpload(ctx context.Context, st *store.Store, deleteRemoved bool) error {
	be, err := buildBackend(ctx, st, "")
	if err != nil {
		return err
	}
	return cli.Upload(ctx, st, be, deleteRemoved, os.Stdout)
}

// runStart builds the effective backend and launches the watcher daemon, which
// blocks until interrupted or stopped.
func runStart(ctx context.Context, st *store.Store, cfg *config.Config, deleteRemoved bool) error {
	be, err := buildBackend(ctx, st, "")
	if err != nil {
		return err
	}
	return cli.Start(ctx, st, be, deleteRemoved, cfg.Dir, os.Stdout)
}

// runServe builds the effective backend and launches the localhost web UI.
func runServe(ctx context.Context, st *store.Store) error {
	be, err := buildBackend(ctx, st, "")
	if err != nil {
		return err
	}
	return cli.Serve(ctx, st, be, os.Stdout)
}

// serviceProbe is the `bb status` service probe: the systemd user unit's state
// on Linux, nil elsewhere. It is bounded by a short timeout so a hung
// systemctl cannot stall `bb status` (which bare `bb` also runs).
func serviceProbe() cli.ServiceProbe {
	if runtime.GOOS != "linux" {
		return nil
	}
	return func(ctx context.Context) string {
		mgr, err := service.NewManager()
		if err != nil {
			return ""
		}
		ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		return mgr.State(ctx)
	}
}

// errFlagsReported signals a flag-parse failure the flag package has already
// printed, so run exits 1 without printing it a second time (like the other
// commands' `return 1` on parseFlags errors).
var errFlagsReported = errors.New("flags already reported")

// serviceUsage is the `bb service` synopsis shown on a bad subcommand.
const serviceUsage = "usage: bb service install [--folder DIR] [--delete] [--force] [--linger] | uninstall | status"

// runService dispatches `bb service install|uninstall|status` (systemd user
// unit for the watcher daemon; Linux only).
func runService(ctx context.Context, st *store.Store, cfg *config.Config, opts serviceOpts) error {
	mgr, err := service.NewManager()
	if err != nil {
		return err
	}
	switch opts.sub {
	case "install":
		home, e := os.UserHomeDir()
		if e != nil {
			return fmt.Errorf("%w: home dir: %v", apperr.ErrService, e)
		}
		return cli.ServiceInstall(ctx, st, mgr, opts.folder, home, cfg.Dir, opts.deleteRemoved, opts.force, opts.linger, os.Stdout)
	case "uninstall":
		return cli.ServiceUninstall(ctx, mgr, os.Stdout)
	default: // "status" (parseServiceArgs admits nothing else)
		return cli.ServiceStatus(ctx, mgr, os.Stdout)
	}
}

// serviceOpts is a parsed `bb service` command line.
type serviceOpts struct {
	sub           string // install | uninstall | status
	folder        string // install: --folder ("" = ~/Documents)
	deleteRemoved bool   // install: --delete
	force         bool   // install: --force
	linger        bool   // install: --linger
}

// parseServiceArgs validates `bb service` arguments without touching systemd,
// returning a usage error for an unknown/missing subcommand or stray arguments,
// and errFlagsReported when the flag package has already printed a flag error.
func parseServiceArgs(args []string) (serviceOpts, error) {
	usageErr := fmt.Errorf("%s", serviceUsage)
	if len(args) == 0 {
		return serviceOpts{}, usageErr
	}
	opts := serviceOpts{sub: args[0]}
	switch opts.sub {
	case "install":
		fs := flag.NewFlagSet("service install", flag.ContinueOnError)
		fs.StringVar(&opts.folder, "folder", "", "folder to back up (default ~/Documents)")
		fs.BoolVar(&opts.deleteRemoved, "delete", false, "also remove remote objects whose local files were deleted")
		fs.BoolVar(&opts.force, "force", false, "replace an existing backuprepo.service not created by bb")
		fs.BoolVar(&opts.linger, "linger", false, "also enable systemd lingering (run at boot and after logout)")
		pos, err := parseFlags(fs, args[1:])
		if err != nil {
			return serviceOpts{}, errFlagsReported // flag already printed the problem
		}
		if len(pos) > 0 {
			return serviceOpts{}, usageErr
		}
	case "uninstall", "status":
		if len(args) > 1 {
			return serviceOpts{}, usageErr
		}
	default:
		return serviceOpts{}, usageErr
	}
	return opts, nil
}

// validBackend reports whether kind names a backend ("" means "not given").
// run checks it before any state exists, so `bb ls --backend bogus` and
// `bb backend bogus` fail without creating ~/backup_repo.
func validBackend(kind string) bool { return kind == "" || kind == "s3" || kind == "b2" }

// effectiveBackend resolves the backend kind: flag override → stored → "s3".
func effectiveBackend(ctx context.Context, st *store.Store, override string) (string, error) {
	if override != "" {
		if override != "s3" && override != "b2" {
			return "", apperr.ErrInvalidBackend
		}
		return override, nil
	}
	return st.GetBackend(ctx)
}

// b2ConfigFromStore maps the stored config into a b2.Config.
func b2ConfigFromStore(ctx context.Context, st *store.Store) (b2.Config, error) {
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		return b2.Config{}, err
	}
	return b2.Config{
		Endpoint: cfg.Endpoint, Region: cfg.Region,
		BucketName: cfg.Bucket, BucketID: cfg.BucketID,
		KeyID: cfg.KeyID, AppKey: cfg.AppKey,
	}, nil
}

// buildBackend constructs the selected backend from stored config.
func buildBackend(ctx context.Context, st *store.Store, override string) (b2.Backend, error) {
	kind, err := effectiveBackend(ctx, st, override)
	if err != nil {
		return nil, err
	}
	cfg, err := b2ConfigFromStore(ctx, st)
	if err != nil {
		return nil, err
	}
	return b2.NewBackend(ctx, kind, cfg)
}

// usage writes the command help text to w.
func usage(w io.Writer) {
	fmt.Fprint(w, `bb — back up folders to Backblaze B2 (S3-compatible or native B2 API)

USAGE
  bb <command> [args] [flags]
  (no command = status)

SETUP
  init                       Interactive setup: credentials, bucket name, bucket ID, endpoint, region, first folder
  config                     Show current configuration (app key masked) + active backend
  version                    Print the build version as "bb X.Y.Z" (also --version, -v)
  help                       Show this help (also -h, --help, or <command> -h)
  bucket [<name> [<id>]]     Show, or switch to another bucket (keeps credentials). With <name> only,
                               the bucket ID is auto-resolved from the account; pass <id> to set it explicitly
  buckets                    List all buckets in the account with their IDs (always via the native B2 API)
  appkey [<new-keyID>]       Replace the stored application key — reads the secret from stdin (keeps it out
                               of shell history); pass a new keyID to rotate the whole pair

BACKUP  (watch local folders, upload changed files)
  watch <dir> [--remote P]   Add a folder to the watch list. --remote stores its files under bucket
                               folder P (P/sub/file) instead of under their full local path; on a
                               folder already watched it changes the prefix
  unwatch <dir>              Remove a folder from the watch list
  list                       List watched folders + tracked files (last-backup times)
  status                     Configured? backend, watched folders, pending uploads, daemon (+ service) state
  upload [--delete]          Upload all changed files now (no-op if nothing changed)
                               --delete also removes remote objects whose local files were deleted

DAEMON  (background watcher: real-time fsnotify events + 5-min fallback scan)
  start [--delete]           Watch all folders and back up changes until stopped (foreground)
                               --delete propagates local deletions to the remote (destructive)
  stop                       Signal a running daemon to shut down gracefully
  serve                      Start the localhost web UI on http://127.0.0.1:9171 (foreground)

SERVICE  (Linux: run the daemon in the background as a systemd user service)
  service install [--folder DIR] [--delete] [--force] [--linger]
                             Watch DIR (default ~/Documents), then install, enable and start
                               backuprepo.service so backups run automatically at every login;
                               --linger also runs it at boot and after logout (loginctl enable-linger)
  service uninstall          Stop, disable and remove the service (watched folders are kept)
  service status             Show whether the service is installed, enabled and running

STORAGE BACKEND  (stored mode — used by upload, start, serve and all file operations)
  backend [s3|b2]            Show or set the backend. Default: s3
                               s3 = S3-compatible API (aws-sdk)    b2 = native Backblaze B2 API
                             Override it for one ls / get / put / rm / find with  --backend s3|b2

MANUAL FILE OPERATIONS  (act directly on the bucket)
  ls [path] [-r]             List bucket contents (folders shown with trailing /)
  get <remote> [local] [-r]  Download an object, or a folder with -r
  put <local> [remote] [-r]  Upload a file, or a directory with -r (a failed file is reported but
                               doesn't abort the batch); --skip-existing skips files already uploaded
  rm <path> [-r] [-f]        Delete an object/folder (confirms unless -f/-y)
  find <query> [prefix]      Case-insensitive substring search of object names

FLAGS
  --backend s3|b2            override the stored backend for one ls / get / put / rm / find
  -r                         recursive (ls / get / put / rm)
  -f, -y                     skip the rm confirmation prompt

EXAMPLES
  # First run + backup (default S3 mode)
  bb init
  bb watch ~/Documents
  bb upload

  # Back up ~/Documents automatically in the background (Linux, systemd)
  bb service install
  bb service status

  # Native Backblaze B2 mode + manual file management
  bb backend b2
  bb ls -r
  bb put ./report.pdf reports/report.pdf
  bb get reports/report.pdf ./out.pdf
  bb find report
  bb rm reports/ -r
  # one-off override without changing the stored mode:
  bb ls --backend s3

State: ~/backup_repo/ (backup.db, key).   Exit codes: 0 ok, 1 error (message on stderr).   Docs: README.md
`)
}

// flagSet reports whether the flag name was given on the command line (even
// with an empty value), as opposed to left at its default.
func flagSet(fs *flag.FlagSet, name string) bool {
	set := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// fail prints err to stderr and returns exit code 1.
func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
