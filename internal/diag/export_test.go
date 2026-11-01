package diag

import (
	"archive/zip"
	"bytes"
	"io"
)

// zipContents returns a zip's entries as "name\ncontent" strings, in archive order.
func zipContents(path string) ([]string, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	out := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		rc, err := file.Open()
		if err != nil {
			return nil, err
		}
		var buf bytes.Buffer
		if _, err := io.Copy(&buf, rc); err != nil {
			rc.Close()
			return nil, err
		}
		rc.Close()
		out = append(out, file.Name+"\n"+buf.String())
	}
	return out, nil
}
