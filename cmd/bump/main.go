// Command bump advances the repo's VERSION file by one odometer step.
//
// Usage: go run ./cmd/bump [major|minor|patch]   (default: patch)
//
// It reads ./VERSION, computes the next version per the scheme in
// internal/version (each component wraps 0..20 with carry, e.g. 1.0.20 ->
// 1.1.0), writes it back, and prints "old -> new".
package main

import (
	"fmt"
	"os"

	"backuprepo/internal/version"
)

// versionFile is the path to the single-source-of-truth version file.
const versionFile = "VERSION"

// main bumps VERSION and reports the change, exiting non-zero on any error.
func main() {
	part := "patch"
	if len(os.Args) > 1 {
		part = os.Args[1]
	}
	if err := run(part); err != nil {
		fmt.Fprintln(os.Stderr, "bump:", err)
		os.Exit(1)
	}
}

// run reads VERSION, bumps the given part, and writes the result back.
func run(part string) error {
	data, err := os.ReadFile(versionFile)
	if err != nil {
		return err
	}
	cur, err := version.Parse(string(data))
	if err != nil {
		return err
	}
	next, err := cur.Bump(part)
	if err != nil {
		return err
	}
	if err := os.WriteFile(versionFile, []byte(next.String()+"\n"), 0o644); err != nil {
		return err
	}
	fmt.Printf("%s -> %s\n", cur, next)
	return nil
}
