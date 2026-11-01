package diag

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Export writes a zip holding every log file under logs/ and the caller's manifest as
// manifest.json. The log files are already redacted; Export adds nothing that was not written.
func (l *Log) Export(ctx context.Context, w io.Writer, manifest map[string]any) error {
	if w == nil {
		return errors.New("diag: no export writer")
	}
	l.flushNow()
	names, err := l.fileNames()
	if err != nil {
		return err
	}
	zw := zip.NewWriter(w)
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := addFileToZip(ctx, zw, "logs/"+name, filepath.Join(l.dir, name)); err != nil {
			return err
		}
	}
	if manifest == nil {
		manifest = map[string]any{}
	}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return fmt.Errorf("diag: encode manifest: %w", err)
	}
	entry, err := zw.Create("manifest.json")
	if err != nil {
		return fmt.Errorf("diag: create manifest: %w", err)
	}
	if _, err := entry.Write(encoded); err != nil {
		return fmt.Errorf("diag: write manifest: %w", err)
	}
	if err := zw.Close(); err != nil {
		return fmt.Errorf("diag: close archive: %w", err)
	}
	return nil
}

// addFileToZip streams one file into the archive, so a log file is never held in memory.
func addFileToZip(ctx context.Context, zw *zip.Writer, name, path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("diag: open %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("diag: stat %s: %w", path, err)
	}
	header, err := zip.FileInfoHeader(info)
	if err != nil {
		return fmt.Errorf("diag: header %s: %w", name, err)
	}
	header.Name = name
	header.Method = zip.Deflate
	entry, err := zw.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("diag: create %s: %w", name, err)
	}
	if _, err := io.Copy(entry, ctxReader{ctx: ctx, r: f}); err != nil {
		return fmt.Errorf("diag: copy %s: %w", name, err)
	}
	return nil
}

// ctxReader stops a copy when the caller cancels.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}
