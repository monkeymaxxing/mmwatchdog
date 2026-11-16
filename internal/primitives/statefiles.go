package primitives

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// ReadBody reads a body of at most limit bytes.
func ReadBody(body io.Reader, limit int) ([]byte, error) {
	b, e := io.ReadAll(io.LimitReader(body, int64(limit)+1))
	if e == nil && len(b) > limit {
		e = fmt.Errorf("request exceeds %d bytes", limit)
	}
	return b, e
}

// WriteFileAtomic replaces path with data, readable only by the daemon.
func WriteFileAtomic(path string, data []byte) error {
	f, e := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path))
	if e != nil {
		return e
	}
	defer func() {
		_ = os.Remove(f.Name()) // Failure only leaves an unused temporary file; write, sync, close, rename, and directory sync are checked separately.
	}()
	if _, e = f.Write(data); e == nil {
		e = f.Sync()
	}
	if err := f.Close(); e == nil {
		e = err
	}
	if e == nil {
		e = os.Rename(f.Name(), path)
	}
	if e != nil {
		return e
	}
	dir, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	defer func() {
		_ = dir.Close() // Closing this read-only handle has no pending writes to commit.
	}()
	return dir.Sync()
}
