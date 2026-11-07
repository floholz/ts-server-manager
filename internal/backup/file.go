package backup

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// WriteFile writes f to path via a temp file in the same directory and a
// rename, so an interrupted write never leaves a truncated backup. The file
// is created with mode 0600. Temp files are named "."+base+".tmp-*".
func WriteFile(path string, f *File) error {
	return writeFileAtomic(path, func(w io.Writer) error { return Write(w, f) })
}

func writeFileAtomic(path string, write func(io.Writer) error) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name())
		}
	}()
	if err := tmp.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}
	w := bufio.NewWriter(tmp)
	if err := write(w); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close: %w", err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return errors.Join(fmt.Errorf("rename into place: %w", err), os.Remove(tmp.Name()))
	}
	return nil
}
