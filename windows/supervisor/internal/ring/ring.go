// Package ring is the always-on event log for ncwin. Everything observable
// (child lifecycle, cron runs, watcher events, scans, errors) is appended here
// from service start; probes query a window after the fact instead of arming
// a trace.
package ring

import (
	"sync"
	"time"
)

// Event is one recorded occurrence. Seq is monotonically increasing and never
// reused, so a reader can resume from the last Seq it saw and detect eviction
// (first returned Seq > requested start).
type Event struct {
	Seq    uint64            `json:"seq"`
	Time   time.Time         `json:"time"`
	Kind   string            `json:"kind"`   // e.g. "proc.start", "cron.end", "watch.change"
	Source string            `json:"source"` // component name
	Msg    string            `json:"msg,omitempty"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Buffer is a fixed-capacity circular log. The zero value is not usable; call New.
type Buffer struct {
	mu   sync.RWMutex
	buf  []Event
	next uint64 // Seq of the next event to append; events [next-len, next) are held
	n    int    // number of valid events (<= cap)
}

func New(capacity int) *Buffer {
	return &Buffer{buf: make([]Event, capacity), next: 1}
}

// Add appends an event, evicting the oldest when full, and returns its Seq.
func (b *Buffer) Add(kind, source, msg string, fields map[string]string) uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	seq := b.next
	b.buf[int(seq%uint64(len(b.buf)))] = Event{Seq: seq, Time: time.Now(), Kind: kind, Source: source, Msg: msg, Fields: fields}
	b.next++
	if b.n < len(b.buf) {
		b.n++
	}
	return seq
}

// Bounds returns the oldest held Seq and the next Seq to be assigned.
func (b *Buffer) Bounds() (oldest, next uint64) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.next - uint64(b.n), b.next
}

// Query is a read filter. Zero fields mean "no constraint".
type Query struct {
	FromSeq uint64    // inclusive
	ToSeq   uint64    // exclusive; 0 = up to newest
	Since   time.Time // inclusive
	Kinds   []string  // exact kind or prefix ending in "." (e.g. "watch.")
	Limit   int       // max events returned (oldest first); 0 = all
}

// Read returns events matching q, oldest first.
func (b *Buffer) Read(q Query) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()
	oldest := b.next - uint64(b.n)
	from, to := max(q.FromSeq, oldest), b.next
	if q.ToSeq != 0 && q.ToSeq < to {
		to = q.ToSeq
	}
	var out []Event
	for s := from; s < to; s++ {
		e := b.buf[int(s%uint64(len(b.buf)))]
		if !q.Since.IsZero() && e.Time.Before(q.Since) {
			continue
		}
		if !matchKind(e.Kind, q.Kinds) {
			continue
		}
		out = append(out, e)
		if q.Limit > 0 && len(out) == q.Limit {
			break
		}
	}
	return out
}

func matchKind(kind string, kinds []string) bool {
	if len(kinds) == 0 {
		return true
	}
	for _, k := range kinds {
		if k == kind || (len(k) > 0 && k[len(k)-1] == '.' && len(kind) >= len(k) && kind[:len(k)] == k) {
			return true
		}
	}
	return false
}
