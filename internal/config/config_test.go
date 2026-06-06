package config

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLoadCreatesKeyAndDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home) // windows

	cfg, err := Load(context.Background())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.Key) != 32 {
		t.Fatalf("key len = %d, want 32", len(cfg.Key))
	}
	if _, err := os.Stat(cfg.DBPath); err == nil {
		t.Fatal("DB should not be created by config.Load")
	}
	info, err := os.Stat(cfg.KeyPath)
	if err != nil {
		t.Fatalf("key file missing: %v", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("key perms = %v, want 0600", info.Mode().Perm())
	}
	if filepath.Dir(cfg.KeyPath) != cfg.Dir {
		t.Fatal("key not under backup dir")
	}
}

func TestLoadReusesExistingKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	first, err := Load(context.Background())
	if err != nil {
		t.Fatalf("first Load: %v", err)
	}
	second, err := Load(context.Background())
	if err != nil {
		t.Fatalf("second Load: %v", err)
	}
	if !bytes.Equal(first.Key, second.Key) {
		t.Fatal("key changed between loads")
	}
}
