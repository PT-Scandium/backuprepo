// Command backuprepo uploads changed files from watched folders to a Backblaze
// B2 bucket via the S3-compatible API. This binary covers the core CLI; the
// background daemon and web UI are separate follow-up work.
package main

import (
	"context"
	"flag"
	"fmt"
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

func usage(w *os.File) {
	fmt.Fprint(w, `backuprepo - back up watched folders to Backblaze B2

Usage:
  backuprepo init                 Interactive setup (credentials, bucket, folder)
  backuprepo watch /path/to/dir   Add a folder to the watch list
  backuprepo unwatch /path/to/dir Remove a folder from the watch list
  backuprepo list                 List watched folders and tracked files
  backuprepo status               Show configuration and pending uploads
  backuprepo upload               Upload all changed files now
  backuprepo config               Show current configuration (secret masked)
  backuprepo ls [path] [-r]       List bucket contents (folders shown with trailing /)
  backuprepo get <remote> [local] [-r]   Download an object or (with -r) a folder
  backuprepo put <local> [remote] [-r]   Upload a file or (with -r) a directory
  backuprepo rm <path> [-r] [-f]  Delete an object/folder (confirms unless -f)
  backuprepo find <query> [prefix]  Search object names (substring, case-insensitive)
  backuprepo backend [s3|b2]      Show or set the storage backend
`)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
