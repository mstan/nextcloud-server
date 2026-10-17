// Package weblog turns the web server's JSON access log into ring events, so
// a failed request (e.g. a 423 Locked) can be attributed by time against
// everything else in the ring.
package weblog

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/mstan/nextcloud-server/windows/supervisor/internal/ring"
)

// Kind is the ring event kind recorded for each kept request.
const Kind = "web.req"

type entry struct {
	TS      float64 `json:"ts"`
	Request struct {
		RemoteIP string `json:"remote_ip"`
		Method   string `json:"method"`
		URI      string `json:"uri"`
	} `json:"request"`
	Duration float64 `json:"duration"`
	Size     int64   `json:"size"`
	Status   int     `json:"status"`
}

// Parse decodes one Caddy JSON access-log line. It reports ok=false for
// lines that are not access-log entries, and keep=false for requests not
// worth recording: successful GET, HEAD and PROPFIND requests dominate the
// traffic, so everything else (writes, locks, moves, deletes) and every
// status >= 400 is kept. The query string is dropped (it can carry tokens).
func Parse(line []byte) (msg string, fields map[string]string, keep, ok bool) {
	var e entry
	if err := json.Unmarshal(line, &e); err != nil || e.Request.Method == "" {
		return "", nil, false, false
	}
	path := e.Request.URI
	if i := strings.IndexByte(path, '?'); i >= 0 {
		path = path[:i]
	}
	switch e.Request.Method {
	case "GET", "HEAD", "PROPFIND":
		keep = e.Status >= 400
	default:
		keep = true
	}
	sec := int64(e.TS)
	at := time.Unix(sec, int64((e.TS-float64(sec))*1e9)).UTC().Format("15:04:05.000")
	fields = map[string]string{
		"method": e.Request.Method,
		"path":   path,
		"status": strconv.Itoa(e.Status),
		"ms":     strconv.FormatInt(int64(e.Duration*1000+0.5), 10),
		"size":   strconv.FormatInt(e.Size, 10),
		"remote": e.Request.RemoteIP,
		"end_at": at,
	}
	return e.Request.Method + " " + path, fields, keep, true
}

// Tail follows the access log at path until ctx is done and records kept
// requests in r. It starts at the current end of an existing file (earlier
// requests were recorded by the previous run) and at the start of a file that
// appears later. The file is opened only while reading, so the web server can
// rotate it freely.
func Tail(ctx context.Context, path string, r *ring.Buffer) {
	var offset int64
	if st, err := os.Stat(path); err == nil {
		offset = st.Size()
	}
	var partial []byte
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		offset, partial = readNew(path, offset, partial, r)
	}
}

func readNew(path string, offset int64, partial []byte, r *ring.Buffer) (int64, []byte) {
	f, err := os.Open(path)
	if err != nil {
		return offset, partial
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return offset, partial
	}
	if st.Size() < offset { // rotated or truncated
		offset, partial = 0, nil
	}
	if st.Size() == offset {
		return offset, partial
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, partial
	}
	buf, err := io.ReadAll(io.LimitReader(f, st.Size()-offset))
	if err != nil && len(buf) == 0 {
		return offset, partial
	}
	offset += int64(len(buf))
	buf = append(partial, buf...)
	for {
		i := bytes.IndexByte(buf, '\n')
		if i < 0 {
			break
		}
		if msg, fields, keep, ok := Parse(buf[:i]); ok && keep {
			r.Add(Kind, "web", msg, fields)
		}
		buf = buf[i+1:]
	}
	return offset, append([]byte(nil), buf...)
}
