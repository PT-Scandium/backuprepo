// Package store is backuprepo's SQLite-backed persistence layer. Credential
// fields are AES-GCM sealed before insertion and opened on read, so secrets
// are never written in plaintext.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite"

	"backuprepo/internal/apperr"
	"backuprepo/internal/crypto"
)

// Store wraps the SQLite handle and the master key used for field encryption.
type Store struct {
	db  *sql.DB
	key []byte
}

// S3Config is the decrypted destination configuration.
type S3Config struct {
	Endpoint string
	Region   string
	Bucket   string
	KeyID    string
	AppKey   string
}

// FileRecord is a tracked file's backup state. LastBackup is nil if never uploaded.
type FileRecord struct {
	Path       string
	Size       int64
	ModTime    int64
	SHA256     string
	LastBackup *int64
	RemoteKey  string
}

const schema = `
CREATE TABLE IF NOT EXISTS config (
  id           INTEGER PRIMARY KEY CHECK (id = 1),
  s3_endpoint  TEXT, s3_region TEXT, bucket_name TEXT,
  key_id_enc   BLOB, app_key_enc BLOB, created_at INTEGER
);
CREATE TABLE IF NOT EXISTS folders (
  path TEXT PRIMARY KEY, added_at INTEGER
);
CREATE TABLE IF NOT EXISTS files (
  path TEXT PRIMARY KEY, size INTEGER, mod_time INTEGER,
  sha256 TEXT, last_backup INTEGER, remote_key TEXT
);`

// Open opens (creating if needed) the SQLite DB at path and applies the schema.
func Open(ctx context.Context, path string, key []byte) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("%w: open: %v", apperr.ErrStore, err)
	}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("%w: migrate: %v", apperr.ErrStore, err)
	}
	return &Store{db: db, key: key}, nil
}

// Close releases the database handle.
func (s *Store) Close() error { return s.db.Close() }

// SaveConfig encrypts the credentials and upserts the single config row.
func (s *Store) SaveConfig(ctx context.Context, cfg S3Config) error {
	keyEnc, err := crypto.Seal(s.key, []byte(cfg.KeyID))
	if err != nil {
		return err
	}
	appEnc, err := crypto.Seal(s.key, []byte(cfg.AppKey))
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO config (id, s3_endpoint, s3_region, bucket_name, key_id_enc, app_key_enc, created_at)
		VALUES (1, ?, ?, ?, ?, ?, strftime('%s','now'))
		ON CONFLICT(id) DO UPDATE SET
		  s3_endpoint=excluded.s3_endpoint, s3_region=excluded.s3_region,
		  bucket_name=excluded.bucket_name, key_id_enc=excluded.key_id_enc,
		  app_key_enc=excluded.app_key_enc`,
		cfg.Endpoint, cfg.Region, cfg.Bucket, keyEnc, appEnc)
	if err != nil {
		return fmt.Errorf("%w: save config: %v", apperr.ErrStore, err)
	}
	return nil
}

// GetConfig returns the decrypted config, or ErrNotConfigured if none exists.
func (s *Store) GetConfig(ctx context.Context) (S3Config, error) {
	var cfg S3Config
	var keyEnc, appEnc []byte
	err := s.db.QueryRowContext(ctx,
		`SELECT s3_endpoint, s3_region, bucket_name, key_id_enc, app_key_enc FROM config WHERE id=1`).
		Scan(&cfg.Endpoint, &cfg.Region, &cfg.Bucket, &keyEnc, &appEnc)
	if errors.Is(err, sql.ErrNoRows) {
		return S3Config{}, apperr.ErrNotConfigured
	}
	if err != nil {
		return S3Config{}, fmt.Errorf("%w: get config: %v", apperr.ErrStore, err)
	}
	keyID, err := crypto.Open(s.key, keyEnc)
	if err != nil {
		return S3Config{}, err
	}
	appKey, err := crypto.Open(s.key, appEnc)
	if err != nil {
		return S3Config{}, err
	}
	cfg.KeyID, cfg.AppKey = string(keyID), string(appKey)
	return cfg, nil
}

// IsConfigured reports whether a config row exists.
func (s *Store) IsConfigured(ctx context.Context) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM config WHERE id=1`).Scan(&n); err != nil {
		return false, fmt.Errorf("%w: %v", apperr.ErrStore, err)
	}
	return n > 0, nil
}

// AddFolder records a watched folder (idempotent).
func (s *Store) AddFolder(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO folders (path, added_at) VALUES (?, strftime('%s','now'))
		 ON CONFLICT(path) DO NOTHING`, path)
	if err != nil {
		return fmt.Errorf("%w: add folder: %v", apperr.ErrStore, err)
	}
	return nil
}

// RemoveFolder deletes a watched folder, or ErrFolderNotWatched if absent.
func (s *Store) RemoveFolder(ctx context.Context, path string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM folders WHERE path=?`, path)
	if err != nil {
		return fmt.Errorf("%w: remove folder: %v", apperr.ErrStore, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return apperr.ErrFolderNotWatched
	}
	return nil
}

// ListFolders returns watched folder paths sorted ascending.
func (s *Store) ListFolders(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT path FROM folders ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("%w: list folders: %v", apperr.ErrStore, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("%w: scan folder: %v", apperr.ErrStore, err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetFile returns the tracked record for path, or (nil, nil) if untracked.
func (s *Store) GetFile(ctx context.Context, path string) (*FileRecord, error) {
	var r FileRecord
	var lastBackup sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT path, size, mod_time, sha256, last_backup, remote_key FROM files WHERE path=?`, path).
		Scan(&r.Path, &r.Size, &r.ModTime, &r.SHA256, &lastBackup, &r.RemoteKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: get file: %v", apperr.ErrStore, err)
	}
	if lastBackup.Valid {
		r.LastBackup = &lastBackup.Int64
	}
	return &r, nil
}

// UpsertFile inserts or updates a file record by path.
func (s *Store) UpsertFile(ctx context.Context, r FileRecord) error {
	var lastBackup any
	if r.LastBackup != nil {
		lastBackup = *r.LastBackup
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO files (path, size, mod_time, sha256, last_backup, remote_key)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(path) DO UPDATE SET
		  size=excluded.size, mod_time=excluded.mod_time, sha256=excluded.sha256,
		  last_backup=excluded.last_backup, remote_key=excluded.remote_key`,
		r.Path, r.Size, r.ModTime, r.SHA256, lastBackup, r.RemoteKey)
	if err != nil {
		return fmt.Errorf("%w: upsert file: %v", apperr.ErrStore, err)
	}
	return nil
}

// ListFiles returns all tracked file records sorted by path.
func (s *Store) ListFiles(ctx context.Context) ([]FileRecord, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT path, size, mod_time, sha256, last_backup, remote_key FROM files ORDER BY path`)
	if err != nil {
		return nil, fmt.Errorf("%w: list files: %v", apperr.ErrStore, err)
	}
	defer rows.Close()
	var out []FileRecord
	for rows.Next() {
		var r FileRecord
		var lastBackup sql.NullInt64
		if err := rows.Scan(&r.Path, &r.Size, &r.ModTime, &r.SHA256, &lastBackup, &r.RemoteKey); err != nil {
			return nil, fmt.Errorf("%w: scan file: %v", apperr.ErrStore, err)
		}
		if lastBackup.Valid {
			r.LastBackup = &lastBackup.Int64
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
