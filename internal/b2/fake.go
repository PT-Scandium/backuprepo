package b2

import (
	"bytes"
	"context"
	"io"
	"sort"
	"strings"
	"time"

	"backuprepo/internal/apperr"
)

// FakeBackend is an in-memory Backend for tests.
type FakeBackend struct {
	Objects map[string][]byte
}

// NewFake returns an empty in-memory backend.
func NewFake() *FakeBackend {
	return &FakeBackend{Objects: map[string][]byte{}}
}

func (f *FakeBackend) Upload(ctx context.Context, key string, r io.Reader, size int64) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	f.Objects[key] = data
	return nil
}

func (f *FakeBackend) Exists(ctx context.Context, key string) (bool, error) {
	_, ok := f.Objects[key]
	return ok, nil
}

func (f *FakeBackend) Download(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	data, ok := f.Objects[key]
	if !ok {
		return nil, 0, apperr.ErrObjectNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (f *FakeBackend) Delete(ctx context.Context, key string) error {
	if _, ok := f.Objects[key]; !ok {
		return apperr.ErrObjectNotFound
	}
	delete(f.Objects, key)
	return nil
}

func (f *FakeBackend) List(ctx context.Context, prefix string, recursive bool) (Listing, error) {
	var out Listing
	seen := map[string]bool{}
	keys := make([]string, 0, len(f.Objects))
	for k := range f.Objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if recursive {
			out.Objects = append(out.Objects, ObjectInfo{Key: k, Size: int64(len(f.Objects[k])), Modified: time.Time{}})
			continue
		}
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			folder := prefix + rest[:i+1]
			if !seen[folder] {
				seen[folder] = true
				out.Prefixes = append(out.Prefixes, folder)
			}
			continue
		}
		out.Objects = append(out.Objects, ObjectInfo{Key: k, Size: int64(len(f.Objects[k])), Modified: time.Time{}})
	}
	return out, nil
}
