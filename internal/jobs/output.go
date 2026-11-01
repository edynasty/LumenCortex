package jobs

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// Stream is one output stream of a job as a reader sees it: a bounded head, a bounded tail, the byte
// total the command produced, and whether anything between them was cut.
type Stream struct {
	Head      string `json:"head"`
	Tail      string `json:"tail"`
	Total     int64  `json:"total"`
	Truncated bool   `json:"truncated"`
	// Omitted is how many bytes between the returned head and tail were cut: the marker that tells a
	// reader the two parts are not the whole stream.
	Omitted int64 `json:"omitted"`
}

// Output is a job's stdout and stderr as they are now: a running job's partial output is readable
// before it ends.
type Output struct {
	JobID  string `json:"jobId"`
	State  string `json:"state"`
	Stdout Stream `json:"stdout"`
	Stderr Stream `json:"stderr"`
}

// jobOutput writes one job's stdout and stderr to <dir>/stdout.head, stdout.tail, stderr.head and
// stderr.tail, bounded per stream by headLimit+tailLimit bytes on disk and by the same in memory. The
// head is flushed on every write and the tail whenever flushEvery more bytes arrive, so another reader
// sees progress while the job runs. Writes never block the command on a slow reader: the buffers are
// fixed size and the middle of a stream is dropped once they are full.
type jobOutput struct {
	stdout    *outStream
	stderr    *outStream
	closeOnce sync.Once
	closeErr  error
}

type outStream struct {
	mu        sync.Mutex
	headLimit int
	tailLimit int
	head      []byte
	tail      []byte
	total     int64
	pending   int
	headFile  *os.File
	tailFile  *os.File
	truncated bool
}

// newJobOutput creates (or reopens) a job's output files. limit is the per-stream budget; a limit
// smaller than two bytes is raised to the schema's minimum.
func newJobOutput(dir string, limit int) (*jobOutput, error) {
	if limit < 1024 {
		limit = 1024
	}
	// A quarter of the budget is the head and the rest the tail, like the shell runner's 8 KiB / 24 KiB.
	head := limit / 4
	tail := limit - head
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	out := &jobOutput{}
	streams := make([]*outStream, 0, 2)
	for _, name := range []string{"stdout", "stderr"} {
		headFile, err := os.OpenFile(filepath.Join(dir, name+".head"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			out.Close()
			return nil, err
		}
		tailFile, err := os.OpenFile(filepath.Join(dir, name+".tail"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			_ = headFile.Close()
			out.Close()
			return nil, err
		}
		s := &outStream{headLimit: head, tailLimit: tail, headFile: headFile, tailFile: tailFile}
		streams = append(streams, s)
	}
	out.stdout, out.stderr = streams[0], streams[1]
	return out, nil
}

// Write keeps one stream's bounded head and tail; it never fails, so a command's own output handling is
// never disturbed by the job's retention limit.
func (s *outStream) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(p)
	s.total += int64(n)
	if missing := s.headLimit - len(s.head); missing > 0 {
		take := min(missing, len(p))
		if _, err := s.headFile.Write(p[:take]); err != nil {
			return n, nil
		}
		s.head = append(s.head, p[:take]...)
	}
	if s.tailLimit == 0 || len(p) == 0 {
		return n, nil
	}
	if len(p) >= s.tailLimit {
		s.tail = append(s.tail[:0], p[len(p)-s.tailLimit:]...)
		s.truncated = true
	} else {
		combined := len(s.tail) + len(p)
		if combined > s.tailLimit {
			drop := combined - s.tailLimit
			copy(s.tail, s.tail[drop:])
			s.tail = s.tail[:len(s.tail)-drop]
			s.truncated = true
		}
		s.tail = append(s.tail, p...)
	}
	s.pending += n
	if s.pending >= flushEvery {
		if err := s.flushTail(); err != nil {
			return n, nil
		}
	}
	return n, nil
}

// flushTail rewrites the tail file with the retained tail. The file is small (one budget's tail), so a
// rewrite is cheaper than tracking overwritten offsets.
func (s *outStream) flushTail() error {
	if _, err := s.tailFile.Seek(0, 0); err != nil {
		return err
	}
	if err := s.tailFile.Truncate(0); err != nil {
		return err
	}
	if _, err := s.tailFile.Write(s.tail); err != nil {
		return err
	}
	s.pending = 0
	return nil
}

// Read returns the retained head and tail of both streams, each bounded by maxBytes.
func (o *jobOutput) Read(maxBytes int) (Stream, Stream) {
	return o.stdout.read(maxBytes), o.stderr.read(maxBytes)
}

func (s *outStream) read(maxBytes int) Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	headLimit := maxBytes / 4
	tailLimit := maxBytes - headLimit
	head := s.head
	if len(head) > headLimit {
		head = head[:headLimit]
	}
	tail := s.tail
	if len(tail) > tailLimit {
		tail = tail[len(tail)-tailLimit:]
	}
	return Stream{
		Head:      string(head),
		Tail:      string(tail),
		Total:     s.total,
		Truncated: s.truncated || s.total > int64(len(s.head)+len(s.tail)),
		Omitted:   max(0, s.total-int64(len(head)+len(tail))),
	}
}

// Totals returns the byte totals of both streams and whether either lost its middle.
func (o *jobOutput) Totals() (stdout, stderr int64, truncated bool) {
	o.stdout.mu.Lock()
	stdout, stdoutCut := o.stdout.total, o.stdout.truncated
	o.stdout.mu.Unlock()
	o.stderr.mu.Lock()
	stderr, stderrCut := o.stderr.total, o.stderr.truncated
	o.stderr.mu.Unlock()
	return stdout, stderr, stdoutCut || stderrCut
}

// Close flushes the tails and closes the files. It is safe to call more than once.
func (o *jobOutput) Close() error {
	o.closeOnce.Do(func() {
		for _, s := range []*outStream{o.stdout, o.stderr} {
			if s == nil {
				continue
			}
			s.mu.Lock()
			if s.tailFile != nil {
				o.closeErr = errors.Join(o.closeErr, s.flushTail(), s.tailFile.Close())
				s.tailFile = nil
			}
			if s.headFile != nil {
				o.closeErr = errors.Join(o.closeErr, s.headFile.Close())
				s.headFile = nil
			}
			s.mu.Unlock()
		}
	})
	return o.closeErr
}

// readStream reads a finished job's output files, bounded by maxBytes per stream.
func readStream(dir, name string, maxBytes int, total int64, truncated bool) Stream {
	headLimit := maxBytes / 4
	tailLimit := maxBytes - headLimit
	head, _ := os.ReadFile(filepath.Join(dir, name+".head"))
	tail, _ := os.ReadFile(filepath.Join(dir, name+".tail"))
	if len(head) > headLimit {
		head = head[:headLimit]
	}
	if len(tail) > tailLimit {
		tail = tail[len(tail)-tailLimit:]
	}
	return Stream{
		Head:      string(head),
		Tail:      string(tail),
		Total:     total,
		Truncated: truncated || total > int64(len(head)+len(tail)),
		Omitted:   max(0, total-int64(len(head)+len(tail))),
	}
}
