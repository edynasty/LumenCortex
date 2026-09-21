package streambuf

import "sync"

// HeadTail keeps a bounded prefix and suffix while counting every byte written.
// It is designed for unbounded tool output: retained memory is O(head+tail), not O(total output).
type HeadTail struct {
	mu        sync.Mutex
	headLimit int
	tailLimit int
	head      []byte
	tail      []byte
	total     int64
}

type Snapshot struct {
	Head      []byte `json:"head"`
	Tail      []byte `json:"tail"`
	Total     int64  `json:"total"`
	Truncated bool   `json:"truncated"`
}

func New(headLimit, tailLimit int) *HeadTail {
	if headLimit < 0 {
		headLimit = 0
	}
	if tailLimit < 0 {
		tailLimit = 0
	}
	return &HeadTail{headLimit: headLimit, tailLimit: tailLimit}
}

func (b *HeadTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	n := len(p)
	b.total += int64(n)

	if missing := b.headLimit - len(b.head); missing > 0 {
		take := missing
		if take > len(p) {
			take = len(p)
		}
		b.head = append(b.head, p[:take]...)
	}

	if b.tailLimit == 0 || len(p) == 0 {
		return n, nil
	}

	if len(p) >= b.tailLimit {
		b.tail = append(b.tail[:0], p[len(p)-b.tailLimit:]...)
		return n, nil
	}

	combined := len(b.tail) + len(p)
	if combined > b.tailLimit {
		drop := combined - b.tailLimit
		copy(b.tail, b.tail[drop:])
		b.tail = b.tail[:len(b.tail)-drop]
	}
	b.tail = append(b.tail, p...)
	return n, nil
}

func (b *HeadTail) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()

	head := append([]byte(nil), b.head...)
	tail := append([]byte(nil), b.tail...)
	retained := int64(len(head) + len(tail))
	return Snapshot{
		Head:      head,
		Tail:      tail,
		Total:     b.total,
		Truncated: b.total > retained,
	}
}
