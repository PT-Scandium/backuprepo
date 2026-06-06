package main

import (
	"bytes"
	"strings"
	"testing"
)

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
		"--backend",                  // per-command override flag documented
		"backend b2",                 // switching to native B2 mode
		"backuprepo ls --backend s3", // one-off override example
		"-r",                         // recursive flag
		"-f, -y",                     // rm confirmation skip
		"S3-compatible",              // both modes described
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
