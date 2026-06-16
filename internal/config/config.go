// Package config resolves backuprepo's home paths and manages the master key.
// The master key is a random 32-byte file at ~/backup_repo/key (0600); it is
// created on first load and reused afterwards so the daemon can start silently.
package config

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"

	"backuprepo/internal/apperr"
	"backuprepo/internal/crypto"
)

// Config holds resolved paths and the loaded master key.
type Config struct {
	Dir     string // ~/backup_repo
	DBPath  string // ~/backup_repo/backup.db
	KeyPath string // ~/backup_repo/key
	Key     []byte // 32-byte master key
}

// Load resolves paths, creates the backup dir and master key if absent,
// and returns the populated Config. It does NOT create the database.
func Load(ctx context.Context) (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("%w: home dir: %v", apperr.ErrStore, err)
	}
	dir := filepath.Join(home, "backup_repo")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("%w: mkdir %s: %v", apperr.ErrStore, dir, err)
	}
	cfg := &Config{
		Dir:     dir,
		DBPath:  filepath.Join(dir, "backup.db"),
		KeyPath: filepath.Join(dir, "key"),
	}
	key, err := loadOrCreateKey(cfg.KeyPath)
	if err != nil {
		return nil, err
	}
	cfg.Key = key
	return cfg, nil
}

// loadOrCreateKey reads the master key from path, or generates and writes a new
// 32-byte key (0600) if none exists.
func loadOrCreateKey(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err == nil {
		if len(data) != crypto.KeySize {
			return nil, fmt.Errorf("%w: key file corrupt (len %d)", apperr.ErrCrypto, len(data))
		}
		return data, nil
	}
	if !os.IsNotExist(err) {
		return nil, fmt.Errorf("%w: read key: %v", apperr.ErrCrypto, err)
	}
	key := make([]byte, crypto.KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("%w: gen key: %v", apperr.ErrCrypto, err)
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, fmt.Errorf("%w: write key: %v", apperr.ErrCrypto, err)
	}
	return key, nil
}
