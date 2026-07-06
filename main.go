// Command backuprepo uploads changed files from watched folders to a Backblaze
// B2 bucket via the S3-compatible API. This binary covers the core CLI; the
// background daemon and web UI are separate follow-up work.
package main

import (
	"context"
	_ "embed"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/cli"
	"backuprepo/internal/config"
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

// run loads config, opens the store, dispatches the subcommand, and returns an
// exit code (0 success, 1 error).
func run(args []string) int {
	ctx := context.Background()
	cfg, err := config.Load(ctx)
	if err != nil {
		return fail(err)
	}
	st, err := store.Open(ctx, cfg.DBPath, cfg.Key)
	if err != nil {
		return fail(err)
	}
	defer st.Close()

	cmd := "status"
	var rest []string
	if len(args) > 0 {
		cmd, rest = args[0], args[1:]
	}

	switch cmd {
	case "init":
		err = cli.Init(ctx, st, os.Stdin, os.Stdout, b2.ListBuckets)
	case "watch":
		if len(rest) != 1 {
			return fail(fmt.Errorf("usage: bb watch /path/to/dir"))
		}
		err = cli.Watch(ctx, st, rest[0], os.Stdout)
	case "unwatch":
		if len(rest) != 1 {
			return fail(fmt.Errorf("usage: bb unwatch /path/to/dir"))
		}
		err = cli.Unwatch(ctx, st, rest[0], os.Stdout)
	case "list":
		err = cli.List(ctx, st, os.Stdout)
	case "status":
		err = cli.Status(ctx, st, os.Stdout)
	case "config":
		err = cli.Config(ctx, st, os.Stdout)
	case "upload":
		fs := flag.NewFlagSet("upload", flag.ContinueOnError)
		del := fs.Bool("delete", false, "also remove remote objects whose local files were deleted")
		if _, e := parseFlags(fs, rest); e != nil {
			return 1
		}
		err = runUpload(ctx, st, *del)
	case "ls":
		fs := flag.NewFlagSet("ls", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Ls(ctx, be, argAt(pos, 0), *recursive, os.Stdout)
	case "find":
		fs := flag.NewFlagSet("find", flag.ContinueOnError)
		backend := fs.String("backend", "", "override backend (s3|b2)")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		if len(pos) < 1 {
			return fail(fmt.Errorf("usage: bb find <query> [prefix]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Find(ctx, be, argAt(pos, 0), argAt(pos, 1), os.Stdout)
	case "get":
		fs := flag.NewFlagSet("get", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		if len(pos) < 1 {
			return fail(fmt.Errorf("usage: bb get <remote> [local] [-r]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Get(ctx, be, argAt(pos, 0), argAt(pos, 1), *recursive, os.Stdout)
	case "put":
		fs := flag.NewFlagSet("put", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		skipExisting := fs.Bool("skip-existing", false, "skip files already present remotely (resume an interrupted upload)")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		pos, e := parseFlags(fs, rest)
		if e != nil {
			return 1
		}
		if len(pos) < 1 {
			return fail(fmt.Errorf("usage: bb put <local> [remote] [-r] [--skip-existing]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Put(ctx, be, argAt(pos, 0), argAt(pos, 1), *recursive, *skipExisting, os.Stdout)
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
		if len(pos) < 1 {
			return fail(fmt.Errorf("usage: bb rm <path> [-r] [-f]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Rm(ctx, be, argAt(pos, 0), *recursive, *force, os.Stdin, os.Stdout)
	case "backend":
		kind := ""
		if len(rest) > 0 {
			kind = rest[0]
		}
		err = cli.Backend(ctx, st, kind, os.Stdout)
	case "bucket":
		if len(rest) > 2 {
			return fail(fmt.Errorf("usage: bb bucket [<name> [<bucket-id>]]"))
		}
		err = cli.Bucket(ctx, st, argAt(rest, 0), argAt(rest, 1), b2.ListBuckets, os.Stdout)
	case "buckets":
		cfg, e := b2ConfigFromStore(ctx, st)
		if e != nil {
			return fail(e)
		}
		err = cli.Buckets(ctx, cfg, os.Stdout)
	case "appkey":
		if len(rest) > 1 {
			return fail(fmt.Errorf("usage: bb appkey [<new-keyID>]  (secret read from stdin)"))
		}
		err = cli.SetAppKey(ctx, st, argAt(rest, 0), os.Stdin, os.Stdout)
	case "start":
		fs := flag.NewFlagSet("start", flag.ContinueOnError)
		del := fs.Bool("delete", false, "also remove remote objects whose local files were deleted")
		if _, e := parseFlags(fs, rest); e != nil {
			return 1
		}
		err = runStart(ctx, st, cfg, *del)
	case "stop":
		err = cli.Stop(ctx, cfg.Dir, os.Stdout)
	case "serve":
		err = runServe(ctx, st)
	case "version", "--version", "-v":
		fmt.Fprintf(os.Stdout, "bb %s\n", strings.TrimSpace(versionFile))
	case "help", "-h", "--help":
		usage(os.Stdout)
	default:
		usage(os.Stderr)
		return 1
	}
	if err != nil {
		return fail(err)
	}
	return 0
}

// parseFlags parses args while allowing flags and positional arguments to
// appear in any order, returning the positionals. The stdlib flag parser stops
// at the first non-flag token, so we resume parsing after each positional —
// this lets `ls photos/ -r` and `rm brtest/ -r -f` behave like `ls -r photos/`.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positionals []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		rest := fs.Args()
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
  version                    Print the build version (e.g. 1.0.0)
  bucket [<name> [<id>]]     Show, or switch to another bucket (keeps credentials). With <name> only,
                               the bucket ID is auto-resolved from the account; pass <id> to set it explicitly
  buckets                    List all buckets in the account with their IDs (always via the native B2 API)
  appkey [<new-keyID>]       Replace the stored application key — reads the secret from stdin (keeps it out
                               of shell history); pass a new keyID to rotate the whole pair

BACKUP  (watch local folders, upload changed files)
  watch <dir>                Add a folder to the watch list
  unwatch <dir>              Remove a folder from the watch list
  list                       List watched folders + tracked files (last-backup times)
  status                     Configured? backend, watched folders, pending uploads
  upload [--delete]          Upload all changed files now (no-op if nothing changed)
                               --delete also removes remote objects whose local files were deleted

DAEMON  (background watcher: real-time fsnotify events + 5-min fallback scan)
  start [--delete]           Watch all folders and back up changes until stopped (foreground)
  stop                       Signal a running daemon to shut down gracefully
                               --delete propagates local deletions to the remote (destructive)
  serve                      Start the localhost web UI on http://127.0.0.1:9171 (foreground)

STORAGE BACKEND  (mode — applies to upload and all file operations)
  backend [s3|b2]            Show or set the backend. Default: s3
                               s3 = S3-compatible API (aws-sdk)    b2 = native Backblaze B2 API
                             Override for one command with  --backend s3|b2

MANUAL FILE OPERATIONS  (act directly on the bucket)
  ls [path] [-r]             List bucket contents (folders shown with trailing /)
  get <remote> [local] [-r]  Download an object, or a folder with -r
  put <local> [remote] [-r]  Upload a file, or a directory with -r (a failed file is reported but
                               doesn't abort the batch); --skip-existing skips files already uploaded
  rm <path> [-r] [-f]        Delete an object/folder (confirms unless -f/-y)
  find <query> [prefix]      Case-insensitive substring search of object names

FLAGS
  --backend s3|b2            override the backend for one command
  -r                         recursive (ls / get / put / rm)
  -f, -y                     skip the rm confirmation prompt

EXAMPLES
  # First run + backup (default S3 mode)
  bb init
  bb watch ~/Documents
  bb upload

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

// fail prints err to stderr and returns exit code 1.
func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
