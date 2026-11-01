// Package diag is the runtime's diagnostics log (ADR-0019 §5): a slog handler that redacts a
// record before it is queued, a bounded queue that never makes the agent path wait, one writer
// goroutine writing JSON Lines with size rotation and retention, a filtered reader and a redacted
// export.
//
// The log is lossy on purpose. Durable state stays in SQLite; this log exists so a host can read
// what happened and export it for a bug report without leaking prompts, tool output or credentials.
package diag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// Defaults of Open. The configuration's execution.logLevel selects the level; the rest are the
// documented ADR-0019 values.
const (
	DefaultMaxFileBytes  = 8 << 20
	DefaultMaxFiles      = 5
	DefaultQueueSize     = 4096
	DefaultRetentionDays = 14

	// FileName is the current log file inside the log directory; rotated files are
	// lumencortex.1.jsonl (newest) to lumencortex.<MaxFiles-1>.jsonl.
	FileName = "lumencortex.jsonl"

	filePerm = 0o600
	dirPerm  = 0o700

	// flushInterval bounds how long a queued record stays in the writer's buffer.
	flushInterval = time.Second
	// flushThreshold is the buffer size that flushes before the interval.
	flushThreshold = 32 << 10
	// flushWait bounds how long Query and Export wait for the writer to flush.
	flushWait = 250 * time.Millisecond
)

// logNameRE matches the current and rotated log files and nothing else in the directory.
var logNameRE = regexp.MustCompile(`^lumencortex(?:\.(\d+))?\.jsonl$`)

// Options configure one Log.
type Options struct {
	// Dir is the log directory; it is created with mode 0700.
	Dir string
	// Level is the level filter, read at every record so a host can change it without reopening.
	// Nil means info.
	Level *slog.LevelVar
	// MaxFileBytes is the size a file reaches before rotation. Zero means 8 MiB.
	MaxFileBytes int64
	// MaxFiles is how many files the directory keeps, current included. Zero means 5.
	MaxFiles int
	// QueueSize is how many records may wait for the writer. Zero means 4096.
	QueueSize int
	// RetentionDays is how long a rotated file is kept. Zero means 14.
	RetentionDays int
	// Now is the clock; nil means time.Now. Tests use it to age files.
	Now func() time.Time

	// beforeWrite is a test hook the writer calls before each record, to stall it on purpose. It is
	// read only by the writer goroutine, which starts after Open read it.
	beforeWrite func()
}

// Stats is a snapshot of the log's counters and its files on disk. Counters only grow; reading
// them never resets them.
type Stats struct {
	Written  uint64 `json:"written"`
	Dropped  uint64 `json:"dropped"`
	Files    uint64 `json:"files"`
	Bytes    uint64 `json:"bytes"`
	QueueLen uint64 `json:"queueLen"`
	QueueCap uint64 `json:"queueCap"`
}

// Log is one diagnostics log: a slog handler over a bounded queue and one writer goroutine.
type Log struct {
	dir          string
	level        *slog.LevelVar
	maxFileBytes int64
	maxFiles     int
	retention    time.Duration
	now          func() time.Time

	queue chan []byte
	flush chan chan struct{}
	stop  chan struct{}
	done  chan struct{}

	// mu makes Close linearizable with enqueue: a record queued before Close is written, one
	// queued after it is counted as dropped and never reaches a closed channel.
	mu     sync.RWMutex
	closed bool

	closeOnce sync.Once
	closeErr  error

	written atomic.Uint64
	dropped atomic.Uint64

	logger *slog.Logger

	// beforeWrite is a test hook the writer calls before each record, to stall it on purpose.
	beforeWrite func()

	errMu sync.Mutex
	err   error
}

// Open creates dir if needed, deletes files older than the retention window and starts the writer
// goroutine.
func Open(opts Options) (*Log, error) {
	if opts.Dir == "" {
		return nil, errors.New("diag: no log directory")
	}
	l := &Log{
		dir:          opts.Dir,
		level:        opts.Level,
		maxFileBytes: opts.MaxFileBytes,
		maxFiles:     opts.MaxFiles,
		retention:    time.Duration(opts.RetentionDays) * 24 * time.Hour,
		now:          opts.Now,
	}
	if l.level == nil {
		l.level = new(slog.LevelVar)
		l.level.Set(slog.LevelInfo)
	}
	if l.maxFileBytes <= 0 {
		l.maxFileBytes = DefaultMaxFileBytes
	}
	if l.maxFiles <= 0 {
		l.maxFiles = DefaultMaxFiles
	}
	if opts.QueueSize <= 0 {
		opts.QueueSize = DefaultQueueSize
	}
	if l.retention <= 0 {
		l.retention = time.Duration(DefaultRetentionDays) * 24 * time.Hour
	}
	if l.now == nil {
		l.now = time.Now
	}
	if err := os.MkdirAll(l.dir, dirPerm); err != nil {
		return nil, fmt.Errorf("diag: create %s: %w", l.dir, err)
	}
	if err := l.prune(true); err != nil {
		return nil, err
	}
	l.queue = make(chan []byte, opts.QueueSize)
	l.flush = make(chan chan struct{}, 1)
	l.stop = make(chan struct{})
	l.done = make(chan struct{})
	l.beforeWrite = opts.beforeWrite
	l.logger = slog.New(&handler{log: l})
	go l.run()
	return l, nil
}

// Logger returns the slog.Logger backed by the log. It is safe to call from any goroutine and at
// any time, including after Close, where records are dropped.
func (l *Log) Logger() *slog.Logger { return l.logger }

// Level returns the level filter, for a host that changes it at runtime.
func (l *Log) Level() *slog.LevelVar { return l.level }

// Dir returns the log directory.
func (l *Log) Dir() string { return l.dir }

// Stats reads the counters and scans the directory for its file count and size.
func (l *Log) Stats() Stats {
	files, bytes := l.scan()
	return Stats{
		Written:  l.written.Load(),
		Dropped:  l.dropped.Load(),
		Files:    files,
		Bytes:    bytes,
		QueueLen: uint64(len(l.queue)),
		QueueCap: uint64(cap(l.queue)),
	}
}

// Close drains the queue, flushes the buffer and stops the writer. It is idempotent; logger calls
// that race with it are dropped and counted, never written to a closed channel.
func (l *Log) Close() error {
	l.closeOnce.Do(func() {
		l.mu.Lock()
		l.closed = true
		close(l.stop)
		l.mu.Unlock()
		<-l.done
		l.errMu.Lock()
		l.closeErr = l.err
		l.errMu.Unlock()
	})
	return l.closeErr
}

// enqueue hands one encoded record to the writer without ever waiting for it. A full queue drops
// the record and counts it: log I/O must never stall the agent.
func (l *Log) enqueue(line []byte) {
	l.mu.RLock()
	defer l.mu.RUnlock()
	if l.closed {
		l.dropped.Add(1)
		return
	}
	select {
	case l.queue <- line:
	default:
		l.dropped.Add(1)
	}
}

// run is the writer goroutine: it owns the current file and the buffer.
func (l *Log) run() {
	defer close(l.done)
	w := &fileWriter{log: l}
	defer w.close()
	ticker := time.NewTicker(flushInterval)
	defer ticker.Stop()
	for {
		select {
		case line := <-l.queue:
			if l.beforeWrite != nil {
				l.beforeWrite()
			}
			w.record(line)
		case ack := <-l.flush:
			// A reader asked for what is queued now: take it, not only what the buffer holds.
			w.drain(cap(l.queue))
			w.flush()
			close(ack)
		case <-ticker.C:
			w.flush()
		case <-l.stop:
			// Drain what Close linearized into the queue, then flush and stop. Records enqueued
			// after Close are refused by enqueue, so this loop terminates.
			for {
				select {
				case line := <-l.queue:
					w.record(line)
				default:
					w.flush()
					return
				}
			}
		}
	}
}

// drain moves up to max queued records to the file, so a reader sees what was logged before it
// asked. The bound keeps a producer that logs continuously from starving the writer's select.
func (w *fileWriter) drain(max int) {
	for i := 0; i < max; i++ {
		select {
		case line := <-w.log.queue:
			w.record(line)
		default:
			return
		}
	}
}

// flushNow asks the writer to flush its buffer so a reader sees the newest records. It gives up
// after flushWait (a writer that is blocked, e.g. in a test hook) and never blocks Close.
func (l *Log) flushNow() {
	ack := make(chan struct{})
	timer := time.NewTimer(flushWait)
	defer timer.Stop()
	select {
	case l.flush <- ack:
	case <-l.stop:
		return
	case <-timer.C:
		return
	}
	select {
	case <-ack:
	case <-l.stop:
	case <-timer.C:
	}
}

// scan counts the log files and their bytes.
func (l *Log) scan() (files, bytes uint64) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return 0, 0
	}
	for _, entry := range entries {
		if entry.IsDir() || !logNameRE.MatchString(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		files++
		bytes += uint64(info.Size())
	}
	return files, bytes
}

// fileNames returns the log files newest first: the current file, then lumencortex.1.jsonl and so
// on. A file that does not exist is skipped.
func (l *Log) fileNames() ([]string, error) {
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		return nil, fmt.Errorf("diag: read %s: %w", l.dir, err)
	}
	type indexed struct {
		index int
		name  string
	}
	var found []indexed
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		m := logNameRE.FindStringSubmatch(entry.Name())
		if m == nil {
			continue
		}
		index := 0
		if m[1] != "" {
			index, _ = strconv.Atoi(m[1])
		}
		found = append(found, indexed{index: index, name: entry.Name()})
	}
	sort.Slice(found, func(i, j int) bool { return found[i].index < found[j].index })
	names := make([]string, 0, len(found))
	for _, f := range found {
		names = append(names, f.name)
	}
	return names, nil
}

// pathFor names file index: 0 is the current file, i>0 the i-th rotated one.
func (l *Log) pathFor(index int) string {
	if index == 0 {
		return filepath.Join(l.dir, FileName)
	}
	return filepath.Join(l.dir, fmt.Sprintf("lumencortex.%d.jsonl", index))
}

// prune deletes log files older than the retention window. The current file is only considered
// when the log is not writing yet (includeCurrent at Open).
func (l *Log) prune(includeCurrent bool) error {
	cutoff := l.now().Add(-l.retention)
	entries, err := os.ReadDir(l.dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("diag: read %s: %w", l.dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !logNameRE.MatchString(name) {
			continue
		}
		if !includeCurrent && name == FileName {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if info.ModTime().Before(cutoff) {
			if err := os.Remove(filepath.Join(l.dir, name)); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return fmt.Errorf("diag: remove %s: %w", name, err)
			}
		}
	}
	return nil
}

// fail records the first writer error; later records are dropped rather than retried forever.
func (l *Log) fail(err error) {
	if err == nil {
		return
	}
	l.errMu.Lock()
	if l.err == nil {
		l.err = err
	}
	l.errMu.Unlock()
}

// fileWriter owns the current file and its buffer. Only the writer goroutine touches it.
type fileWriter struct {
	log  *Log
	file *os.File
	buf  *bytes.Buffer
	size int64
	err  error
}

// record writes one encoded JSON line, rotating first when it would cross the size bound.
func (w *fileWriter) record(line []byte) {
	if w.err != nil {
		w.log.dropped.Add(1)
		return
	}
	if w.file == nil {
		if err := w.open(); err != nil {
			w.fail(err)
			return
		}
	}
	if w.size > 0 && w.size+int64(len(line))+1 > w.log.maxFileBytes {
		if err := w.rotate(); err != nil {
			w.fail(err)
			return
		}
	}
	if _, err := w.buf.Write(line); err != nil {
		w.fail(err)
		return
	}
	if err := w.buf.WriteByte('\n'); err != nil {
		w.fail(err)
		return
	}
	w.size += int64(len(line)) + 1
	if w.buf.Len() >= flushThreshold {
		w.flush()
	}
	w.log.written.Add(1)
}

// open creates or appends to the current file.
func (w *fileWriter) open() error {
	f, err := os.OpenFile(w.log.pathFor(0), os.O_CREATE|os.O_WRONLY|os.O_APPEND, filePerm)
	if err != nil {
		return fmt.Errorf("diag: open log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("diag: stat log: %w", err)
	}
	w.file = f
	w.buf = bytes.NewBuffer(make([]byte, 0, flushThreshold))
	w.size = info.Size()
	return nil
}

// flush pushes the buffer to the file.
func (w *fileWriter) flush() {
	if w.file == nil || w.buf == nil || w.buf.Len() == 0 {
		return
	}
	if _, err := w.file.Write(w.buf.Bytes()); err != nil {
		w.fail(err)
		return
	}
	w.buf.Reset()
}

// rotate closes the current file, shifts the rotated files down one index, drops the oldest and
// opens a new current file. MaxFiles of 1 keeps the current file bounded by truncating it.
func (w *fileWriter) rotate() error {
	w.flush()
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			return fmt.Errorf("diag: close log: %w", err)
		}
		w.file = nil
	}
	w.buf = nil
	if w.log.maxFiles <= 1 {
		if err := os.Remove(w.log.pathFor(0)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("diag: truncate log: %w", err)
		}
		return w.open()
	}
	oldest := w.log.pathFor(w.log.maxFiles - 1)
	if err := os.Remove(oldest); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("diag: remove %s: %w", oldest, err)
	}
	for i := w.log.maxFiles - 2; i >= 1; i-- {
		from, to := w.log.pathFor(i), w.log.pathFor(i+1)
		if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("diag: rotate %s: %w", from, err)
		}
	}
	if err := os.Rename(w.log.pathFor(0), w.log.pathFor(1)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("diag: rotate %s: %w", w.log.pathFor(0), err)
	}
	if err := w.open(); err != nil {
		return err
	}
	if err := w.log.prune(false); err != nil {
		// A retention failure must not stop logging: the file set is still bounded by MaxFiles.
		w.log.fail(err)
	}
	return nil
}

func (w *fileWriter) close() {
	w.flush()
	if w.file != nil {
		if err := w.file.Close(); err != nil {
			w.fail(fmt.Errorf("diag: close log: %w", err))
		}
		w.file = nil
	}
	w.buf = nil
}

func (w *fileWriter) fail(err error) {
	w.err = err
	w.log.fail(err)
}

// preAttr is an attribute added with Logger.With, together with the group path in force when it
// was added: a group applies only to the attributes added after it.
type preAttr struct {
	path []string
	attr slog.Attr
}

// handler is the slog.Handler: it redacts and encodes on the caller's goroutine, then enqueues
// without blocking.
type handler struct {
	log    *Log
	pre    []preAttr
	groups []string
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.log.level.Level()
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	h.log.enqueue(h.log.encode(r, h.pre, h.groups))
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	out := &handler{log: h.log, pre: make([]preAttr, 0, len(h.pre)+len(attrs)), groups: h.groups}
	out.pre = append(out.pre, h.pre...)
	path := h.groups
	for _, a := range attrs {
		out.pre = append(out.pre, preAttr{path: path, attr: a})
	}
	return out
}

func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	out := &handler{log: h.log, pre: h.pre, groups: make([]string, 0, len(h.groups)+1)}
	out.groups = append(out.groups, h.groups...)
	out.groups = append(out.groups, name)
	return out
}

// node is an insertion-ordered JSON object: time, level and msg stay first in a record, and the
// attributes keep the order the caller attached them. A record has few keys, so a small node scans
// its keys and only a wide one builds an index.
type node struct {
	keys  []string
	vals  []any
	index map[string]int
}

// nodeIndexThreshold is the width above which a node keeps a key index.
const nodeIndexThreshold = 8

func (n *node) child(key string) *node {
	if i, ok := n.lookup(key); ok {
		return n.vals[i].(*node)
	}
	c := &node{}
	n.set(key, c)
	return c
}

func (n *node) lookup(key string) (int, bool) {
	if n.index != nil {
		i, ok := n.index[key]
		return i, ok
	}
	for i, k := range n.keys {
		if k == key {
			return i, true
		}
	}
	return 0, false
}

func (n *node) set(key string, v any) {
	if i, ok := n.lookup(key); ok {
		n.vals[i] = v
		return
	}
	if n.index == nil && len(n.keys) >= nodeIndexThreshold {
		n.index = make(map[string]int, len(n.keys)+1)
		for i, k := range n.keys {
			n.index[k] = i
		}
	}
	if n.index != nil {
		n.index[key] = len(n.keys)
	}
	n.keys = append(n.keys, key)
	n.vals = append(n.vals, v)
}

// addAttr places one attribute in the tree under path, redacting its value first. An inline group
// (an empty key) contributes its attributes to the current level.
func addAttr(n *node, path []string, a slog.Attr, level slog.Level, depth int) {
	a.Value = a.Value.Resolve()
	if a.Equal(slog.Attr{}) {
		return
	}
	for _, g := range path {
		n = n.child(g)
	}
	norm := normalizedKey(a.Key)
	if a.Value.Kind() == slog.KindGroup && !isSecretName(norm) && !contentKeys[norm] {
		// An empty key is an inline group: its attributes belong to the current level.
		target := n
		if a.Key != "" {
			target = n.child(a.Key)
		}
		for _, inner := range a.Value.Group() {
			addAttr(target, nil, inner, level, depth+1)
		}
		return
	}
	n.set(a.Key, sanitizeValue(level, norm, a.Value, depth))
}

// encode renders one record as a JSON line: time, level and msg, then the attributes.
func (l *Log) encode(r slog.Record, pre []preAttr, groups []string) []byte {
	ts := r.Time
	if ts.IsZero() {
		ts = l.now()
	}
	buf := bytes.NewBuffer(make([]byte, 0, 256))
	buf.WriteString(`{"time":`)
	writeJSONString(buf, ts.UTC().Format(time.RFC3339Nano))
	buf.WriteString(`,"level":`)
	writeJSONString(buf, r.Level.String())
	buf.WriteString(`,"msg":`)
	writeJSONString(buf, redactString(r.Message))
	root := &node{}
	for _, p := range pre {
		addAttr(root, p.path, p.attr, r.Level, 0)
	}
	r.Attrs(func(a slog.Attr) bool {
		addAttr(root, groups, a, r.Level, 0)
		return true
	})
	for i, key := range root.keys {
		buf.WriteByte(',')
		writeJSONString(buf, key)
		buf.WriteByte(':')
		writeValue(buf, root.vals[i])
	}
	buf.WriteByte('}')
	if buf.Len() > maxRecordBytes {
		// A record larger than the bound is replaced by a stub: the queue's memory is bounded by
		// QueueSize times maxRecordBytes whatever a caller tried to attach.
		size := buf.Len()
		buf.Reset()
		buf.WriteString(`{"time":`)
		writeJSONString(buf, ts.UTC().Format(time.RFC3339Nano))
		buf.WriteString(`,"level":`)
		writeJSONString(buf, r.Level.String())
		buf.WriteString(`,"msg":`)
		writeJSONString(buf, redactString(r.Message))
		buf.WriteString(`,"truncated":true,"bytes":`)
		buf.WriteString(strconv.Itoa(size))
		buf.WriteByte('}')
	}
	return buf.Bytes()
}

// writeJSONString writes s as a JSON string, escaping the control characters json requires.
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\n':
			buf.WriteString(`\n`)
		case '\r':
			buf.WriteString(`\r`)
		case '\t':
			buf.WriteString(`\t`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				buf.WriteByte(hex[byte(r)>>4])
				buf.WriteByte(hex[byte(r)&0xf])
				continue
			}
			buf.WriteRune(r)
		}
	}
	buf.WriteByte('"')
}

// writeValue writes one attribute value: an ordered node, a string, or anything json.Marshal
// encodes.
func writeValue(buf *bytes.Buffer, v any) {
	switch t := v.(type) {
	case *node:
		buf.WriteByte('{')
		for i, key := range t.keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, key)
			buf.WriteByte(':')
			writeValue(buf, t.vals[i])
		}
		buf.WriteByte('}')
	case string:
		writeJSONString(buf, t)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			writeJSONString(buf, fmt.Sprint(v))
			return
		}
		buf.Write(encoded)
	}
}
