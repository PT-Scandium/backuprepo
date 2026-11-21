#!/bin/sh
# pregate-go-test.sh — test runner for gstack's /verify and /ship pregate.
#
# pregate passes the selected test files; Go tests run per package, so run
# `go test` once on each distinct package directory those files live in.
# Usage: scripts/pregate-go-test.sh internal/cli/cli_test.go main_test.go ...
set -eu

[ "$#" -gt 0 ] || { echo "pregate-go-test: no test files given" >&2; exit 2; }

pkgs=$(for f in "$@"; do
	d=$(dirname "$f")
	if [ "$d" = "." ]; then echo "."; else echo "./$d"; fi
done | sort -u)

# shellcheck disable=SC2086 # word splitting of the package list is intended
exec go test -count=1 $pkgs
