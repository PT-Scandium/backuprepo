// Package apperr holds the shared typed sentinel errors for backuprepo.
// Every package wraps one of these with %w and added context, per the
// "errors are typed, never raw strings" invariant.
package apperr

import "errors"

var (
	ErrNotConfigured      = errors.New("backuprepo: not configured (run `backuprepo init`)")
	ErrAlreadyConfigured  = errors.New("backuprepo: already configured")
	ErrInvalidCredentials = errors.New("backuprepo: invalid credentials")
	ErrFolderNotFound     = errors.New("backuprepo: folder not found")
	ErrFolderNotWatched   = errors.New("backuprepo: folder is not watched")
	ErrUploadFailed       = errors.New("backuprepo: upload failed")
	ErrStore              = errors.New("backuprepo: database error")
	ErrCrypto             = errors.New("backuprepo: encryption error")
)
