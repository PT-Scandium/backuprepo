package b2

import (
	"context"
	"fmt"
	"io"

	"backuprepo/internal/apperr"
)

// uploadLarge handles files above b2SmallFileLimit via the B2 large-file API.
// Implemented in Task 7; until then it returns a clear error rather than
// silently truncating.
func (b *B2Backend) uploadLarge(ctx context.Context, auth *b2Auth, key string, r io.Reader, size int64) error {
	return fmt.Errorf("%w: large-file B2 upload not yet implemented (%s, %d bytes)", apperr.ErrUploadFailed, key, size)
}
