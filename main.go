// Command backuprepo uploads changed files from watched folders to a Backblaze
// B2 bucket via the S3-compatible API. This binary covers the core CLI; the
// background daemon and web UI are separate follow-up work.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"backuprepo/internal/apperr"
	"backuprepo/internal/b2"
	"backuprepo/internal/cli"
	"backuprepo/internal/config"
	"backuprepo/internal/store"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

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
		err = cli.Init(ctx, st, os.Stdin, os.Stdout)
	case "watch":
		if len(rest) != 1 {
			return fail(fmt.Errorf("usage: backuprepo watch /path/to/dir"))
		}
		err = cli.Watch(ctx, st, rest[0], os.Stdout)
	case "unwatch":
		if len(rest) != 1 {
			return fail(fmt.Errorf("usage: backuprepo unwatch /path/to/dir"))
		}
		err = cli.Unwatch(ctx, st, rest[0], os.Stdout)
	case "list":
		err = cli.List(ctx, st, os.Stdout)
	case "status":
		err = cli.Status(ctx, st, os.Stdout)
	case "config":
		err = cli.Config(ctx, st, os.Stdout)
	case "upload":
		err = runUpload(ctx, st)
	case "ls":
		fs := flag.NewFlagSet("ls", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Ls(ctx, be, fs.Arg(0), *recursive, os.Stdout)
	case "find":
		fs := flag.NewFlagSet("find", flag.ContinueOnError)
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail(fmt.Errorf("usage: backuprepo find <query> [prefix]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Find(ctx, be, fs.Arg(0), fs.Arg(1), os.Stdout)
	case "get":
		fs := flag.NewFlagSet("get", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail(fmt.Errorf("usage: backuprepo get <remote> [local] [-r]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Get(ctx, be, fs.Arg(0), fs.Arg(1), *recursive, os.Stdout)
	case "put":
		fs := flag.NewFlagSet("put", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail(fmt.Errorf("usage: backuprepo put <local> [remote] [-r]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Put(ctx, be, fs.Arg(0), fs.Arg(1), *recursive, os.Stdout)
	case "rm":
		fs := flag.NewFlagSet("rm", flag.ContinueOnError)
		recursive := fs.Bool("r", false, "recursive")
		force := fs.Bool("f", false, "skip confirmation")
		fs.BoolVar(force, "y", false, "skip confirmation")
		backend := fs.String("backend", "", "override backend (s3|b2)")
		if err = fs.Parse(rest); err != nil {
			return 1
		}
		if fs.NArg() < 1 {
			return fail(fmt.Errorf("usage: backuprepo rm <path> [-r] [-f]"))
		}
		be, e := buildBackend(ctx, st, *backend)
		if e != nil {
			return fail(e)
		}
		err = cli.Rm(ctx, be, fs.Arg(0), *recursive, *force, os.Stdin, os.Stdout)
	case "backend":
		kind := ""
		if len(rest) > 0 {
			kind = rest[0]
		}
		err = cli.Backend(ctx, st, kind, os.Stdout)
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

func runUpload(ctx context.Context, st *store.Store) error {
	up, err := buildBackend(ctx, st, "")
	if err != nil {
		return err
	}
	return cli.Upload(ctx, st, up, os.Stdout)
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

// buildBackend constructs the selected backend from stored config.
func buildBackend(ctx context.Context, st *store.Store, override string) (b2.Backend, error) {
	kind, err := effectiveBackend(ctx, st, override)
	if err != nil {
		return nil, err
	}
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		return nil, err
	}
	return b2.NewBackend(ctx, kind, b2.Config{
		Endpoint: cfg.Endpoint, Region: cfg.Region,
		BucketName: cfg.Bucket, BucketID: cfg.BucketID,
		KeyID: cfg.KeyID, AppKey: cfg.AppKey,
	})
}

func usage(w io.Writer) {
	fmt.Fprint(w, `backuprepo — back up folders to Backblaze B2 (S3-compatible or native B2 API)

USAGE
  backuprepo <command> [args] [flags]
  (no command = status)

SETUP
  init                       Interactive setup: credentials, bucket name, bucket ID, endpoint, region, first folder
  config                     Show current configuration (app key masked) + active backend

BACKUP  (watch local folders, upload changed files)
  watch <dir>                Add a folder to the watch list
  unwatch <dir>              Remove a folder from the watch list
  list                       List watched folders + tracked files (last-backup times)
  status                     Configured? backend, watched folders, pending uploads
  upload                     Upload all changed files now (no-op if nothing changed)

STORAGE BACKEND  (mode — applies to upload and all file operations)
  backend [s3|b2]            Show or set the backend. Default: s3
                               s3 = S3-compatible API (aws-sdk)    b2 = native Backblaze B2 API
                             Override for one command with  --backend s3|b2

MANUAL FILE OPERATIONS  (act directly on the bucket)
  ls [path] [-r]             List bucket contents (folders shown with trailing /)
  get <remote> [local] [-r]  Download an object, or a folder with -r
  put <local> [remote] [-r]  Upload a file, or a directory with -r
  rm <path> [-r] [-f]        Delete an object/folder (confirms unless -f/-y)
  find <query> [prefix]      Case-insensitive substring search of object names

FLAGS
  --backend s3|b2            override the backend for one command
  -r                         recursive (ls / get / put / rm)
  -f, -y                     skip the rm confirmation prompt

EXAMPLES
  # First run + backup (default S3 mode)
  backuprepo init
  backuprepo watch ~/Documents
  backuprepo upload

  # Native Backblaze B2 mode + manual file management
  backuprepo backend b2
  backuprepo ls -r
  backuprepo put ./report.pdf reports/report.pdf
  backuprepo get reports/report.pdf ./out.pdf
  backuprepo find report
  backuprepo rm reports/ -r
  # one-off override without changing the stored mode:
  backuprepo ls --backend s3

State: ~/backup_repo/ (backup.db, key).   Exit codes: 0 ok, 1 error (message on stderr).   Docs: README.md
`)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
