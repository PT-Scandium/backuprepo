// Command backuprepo uploads changed files from watched folders to a Backblaze
// B2 bucket via the S3-compatible API. This binary covers the core CLI; the
// background daemon and web UI are separate follow-up work.
package main

import (
	"context"
	"fmt"
	"os"

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
	cfg, err := st.GetConfig(ctx)
	if err != nil {
		return err
	}
	up, err := b2.NewBackend(ctx, "s3", b2.Config{
		Endpoint: cfg.Endpoint, Region: cfg.Region, BucketName: cfg.Bucket,
		KeyID: cfg.KeyID, AppKey: cfg.AppKey,
	})
	if err != nil {
		return err
	}
	return cli.Upload(ctx, st, up, os.Stdout)
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
`)
}

func fail(err error) int {
	fmt.Fprintln(os.Stderr, "error:", err)
	return 1
}
