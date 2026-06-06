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
	ErrDownloadFailed     = errors.New("backuprepo: download failed")
	ErrListFailed         = errors.New("backuprepo: list failed")
	ErrDeleteFailed       = errors.New("backuprepo: delete failed")
	ErrObjectNotFound     = errors.New("backuprepo: object not found")
	ErrAuthFailed         = errors.New("backuprepo: authentication failed")
	ErrInvalidBackend     = errors.New("backuprepo: invalid backend (use 's3' or 'b2')")
	ErrStore              = errors.New("backuprepo: database error")
	ErrCrypto             = errors.New("backuprepo: encryption error")
)
