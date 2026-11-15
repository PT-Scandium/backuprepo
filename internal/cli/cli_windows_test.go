//go:build windows

package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWatchCaseInsensitiveOnWindows: NTFS paths are case-insensitive, so a
// re-watch with different letter case must reuse the stored entry, and
// unwatch must match it case-insensitively — also after the folder is gone,
// which only the path comparison (strings.EqualFold) can do.
func TestWatchCaseInsensitiveOnWindows(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "Docs")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	st := newStore(t)
	if err := st.AddFolder(ctx, strings.ToLower(dir)); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Watch(ctx, st, strings.ToUpper(dir), &out); err != nil {
		t.Fatalf("Watch: %v", err)
	}
	if folders, _ := st.ListFolders(ctx); len(folders) != 1 {
		t.Errorf("want 1 entry, got %q", folders)
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := Unwatch(ctx, st, strings.ToUpper(dir), &bytes.Buffer{}); err != nil {
		t.Fatalf("Unwatch after removal: %v", err)
	}
	if folders, _ := st.ListFolders(ctx); len(folders) != 0 {
		t.Errorf("not removed: %q", folders)
	}
}
