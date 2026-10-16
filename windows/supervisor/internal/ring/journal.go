package ring

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"sync"
	"time"
)

// Journal is the on-disk copy of a Buffer: every event as one JSON line,
// flushed at least once a second, so the history survives restarts and
// crashes. At max bytes the file is rotated to a single older generation
// (<path>.1), bounding the disk used to about 2*max.
type Journal struct {
	mu   sync.Mutex
	path string
	max  int64
	f    *os.File
	w    *bufio.Writer
	size int64
	done chan struct{}
}

// OpenJournal opens (appending to) the journal at path.
func OpenJournal(path string, max int64) (*Journal, error) {
	j := &Journal{path: path, max: max, done: make(chan struct{})}
	if err := j.open(); err != nil {
		return nil, err
	}
	go j.flushLoop()
	return j, nil
}

func (j *Journal) open() error {
	f, err := os.OpenFile(j.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	j.f, j.w, j.size = f, bufio.NewWriterSize(f, 64<<10), st.Size()
	return nil
}

func (j *Journal) flushLoop() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-j.done:
			return
		case <-t.C:
			j.mu.Lock()
			if j.w != nil {
				_ = j.w.Flush()
			}
			j.mu.Unlock()
		}
	}
}

// write appends e. A journal that cannot be written loses events on disk
// only; the in-memory ring keeps them.
func (j *Journal) write(e Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	b = append(b, '\n')
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.w == nil {
		return
	}
	if j.size+int64(len(b)) > j.max && j.size > 0 {
		j.rotate()
		if j.w == nil {
			return
		}
	}
	n, _ := j.w.Write(b)
	j.size += int64(n)
}

func (j *Journal) rotate() {
	_ = j.w.Flush()
	_ = j.f.Close()
	j.f, j.w = nil, nil
	_ = os.Remove(j.path + ".1")
	_ = os.Rename(j.path, j.path+".1")
	_ = j.open()
}

// Close flushes and closes the journal.
func (j *Journal) Close() error {
	close(j.done)
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.w == nil {
		return nil
	}
	err := j.w.Flush()
	if cerr := j.f.Close(); err == nil {
		err = cerr
	}
	j.f, j.w = nil, nil
	return err
}

// LoadJournal returns the newest n events of the journal at path (including
// its rotated generation), oldest first. A missing journal is empty; a torn
// last line (crash mid-write) is skipped.
func LoadJournal(path string, n int) ([]Event, error) {
	var out []Event
	for _, p := range []string{path + ".1", path} {
		f, err := os.Open(p)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		err = readEvents(f, func(e Event) {
			out = append(out, e)
			if len(out) > 2*n && n > 0 {
				out = append(out[:0], out[len(out)-n:]...)
			}
		})
		f.Close()
		if err != nil {
			return nil, err
		}
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

func readEvents(r io.Reader, fn func(Event)) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Seq != 0 {
			fn(e)
		}
	}
	return sc.Err()
}

// NewJournaled returns a Buffer seeded with prior events (the newest that
// fit), continuing their sequence numbers, that also writes every new event
// to j.
func NewJournaled(capacity int, j *Journal, prior []Event) *Buffer {
	b := New(capacity)
	if len(prior) > capacity {
		prior = prior[len(prior)-capacity:]
	}
	for _, e := range prior {
		if e.Seq < b.next {
			continue // out of order or duplicated: keep Seq strictly increasing
		}
		// The ring holds one contiguous window [next-n, next); after a gap
		// (events missing from the journal) the window restarts.
		if e.Seq != b.next {
			b.n = 0
		}
		b.buf[int(e.Seq%uint64(len(b.buf)))] = e
		b.next = e.Seq + 1
		if b.n < len(b.buf) {
			b.n++
		}
	}
	b.journal = j
	return b
}

// Search reads the journal at path with q, for when the service (and its
// in-memory ring) is not running.
func Search(path string, q Query) ([]Event, error) {
	all, err := LoadJournal(path, 0)
	if err != nil {
		return nil, err
	}
	var out []Event
	for _, e := range all {
		if e.Seq < q.FromSeq || (q.ToSeq != 0 && e.Seq >= q.ToSeq) {
			continue
		}
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
	return out, nil
}
