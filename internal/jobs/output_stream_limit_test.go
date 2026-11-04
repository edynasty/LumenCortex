package jobs

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// JobOutputBytes is per stream. Both channels may independently retain the
// configured limit; the combined bytes are not a single global quota.
func TestOutputPerStreamDiskBoundWithBothChannels(t *testing.T) {
	const limit = 64 << 10
	dir := t.TempDir()
	out, err := newJobOutput(dir, limit)
	if err != nil {
		t.Fatal(err)
	}
	huge := bytes.Repeat([]byte("x"), 1<<20)
	if _, err := out.stdout.Write(huge); err != nil {
		t.Fatal(err)
	}
	if _, err := out.stderr.Write(huge); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	for _, channel := range []string{"stdout", "stderr"} {
		retained := int64(0)
		for _, suffix := range []string{"head", "tail"} {
			info, err := os.Stat(filepath.Join(dir, channel+"."+suffix))
			if err != nil {
				t.Fatal(err)
			}
			retained += info.Size()
		}
		if retained > limit {
			t.Fatalf("%s retained %d bytes, max %d", channel, retained, limit)
		}
		if retained != limit {
			t.Fatalf("%s retained %d bytes, want full %d head+tail budget", channel, retained, limit)
		}
	}
}
