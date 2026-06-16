// Package apperr holds the shared typed sentinel errors for backuprepo.
// Every package wraps one of these with %w and added context, per the
// "errors are typed, never raw strings" invariant.
package apperr

import "errors"

var (
	ErrNotConfigured      = errors.New("bb: not configured (run `bb init`)")
	ErrAlreadyConfigured  = errors.New("bb: already configured")
	ErrInvalidCredentials = errors.New("bb: invalid credentials")
	ErrFolderNotFound     = errors.New("bb: folder not found")
	ErrFolderNotWatched   = errors.New("bb: folder is not watched")
	ErrUploadFailed       = errors.New("bb: upload failed")
	ErrDownloadFailed     = errors.New("bb: download failed")
	ErrListFailed         = errors.New("bb: list failed")
	ErrDeleteFailed       = errors.New("bb: delete failed")
	ErrObjectNotFound     = errors.New("bb: object not found")
	ErrAuthFailed         = errors.New("bb: authentication failed")
	ErrInvalidBackend     = errors.New("bb: invalid backend (use 's3' or 'b2')")
	ErrStore              = errors.New("bb: database error")
	ErrCrypto             = errors.New("bb: encryption error")
	ErrDaemon             = errors.New("bb: daemon error")
)
