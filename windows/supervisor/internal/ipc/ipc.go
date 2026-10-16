// Package ipc is ncwin's control channel over a named pipe that only SYSTEM
// and Administrators may open. Each connection carries one JSON request.
// Plain ops get one JSON Response. Streaming ops get zero or more Chunk
// objects followed by a Response.
package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Microsoft/go-winio"
)

const PipeName = `\\.\pipe\ncwin`

// SYSTEM and BUILTIN\Administrators get full access; nobody else.
const sddl = "D:P(A;;GA;;;SY)(A;;GA;;;BA)"

type Request struct {
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// Response ends every exchange. Done is always true so readers can tell it from a Chunk.
type Response struct {
	Done   bool            `json:"done"`
	OK     bool            `json:"ok"`
	Error  string          `json:"error,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
}

// Chunk is a piece of streamed output.
type Chunk struct {
	Out string `json:"out"`
}

// Handler answers a plain op.
type Handler func(ctx context.Context, op string, args json.RawMessage) (any, error)

// StreamHandler answers a streaming op, writing output to w as it is produced.
type StreamHandler func(ctx context.Context, args json.RawMessage, w io.Writer) (any, error)

type Server struct {
	Plain   Handler
	Streams map[string]StreamHandler
	// AllowSID, if set, is also granted access. Foreground (`ncwin run`)
	// sessions use it so a non-elevated developer can reach their own pipe.
	AllowSID string
}

// Serve accepts connections until ctx is done.
func (s Server) Serve(ctx context.Context) error {
	sd := sddl
	if s.AllowSID != "" {
		sd += "(A;;GA;;;" + s.AllowSID + ")"
	}
	l, err := winio.ListenPipe(PipeName, &winio.PipeConfig{SecurityDescriptor: sd, InputBufferSize: 64 << 10, OutputBufferSize: 64 << 10})
	if err != nil {
		return fmt.Errorf("listen %s: %w", PipeName, err)
	}
	go func() { <-ctx.Done(); l.Close() }()
	for {
		conn, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			continue
		}
		go s.handle(ctx, conn)
	}
}

type chunkWriter struct {
	mu  sync.Mutex
	enc *json.Encoder
}

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(Chunk{Out: string(p)}); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	enc := json.NewEncoder(conn)
	resp := Response{Done: true}
	var req Request
	var res any
	var err error
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if err = json.NewDecoder(conn).Decode(&req); err != nil {
		err = fmt.Errorf("bad request: %w", err)
	} else if sh, ok := s.Streams[req.Op]; ok {
		_ = conn.SetReadDeadline(time.Time{})
		// Cancel the op if the client goes away.
		cctx, cancel := context.WithCancel(ctx)
		go func() { _, _ = conn.Read(make([]byte, 1)); cancel() }()
		cw := &chunkWriter{enc: enc}
		res, err = sh(cctx, req.Args, cw)
		cancel()
		cw.mu.Lock()
		defer cw.mu.Unlock()
	} else {
		res, err = s.Plain(ctx, req.Op, req.Args)
	}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.OK = true
		resp.Result, _ = json.Marshal(res)
	}
	_ = enc.Encode(resp)
}

func dial() (net.Conn, error) {
	timeout := 3 * time.Second
	conn, err := winio.DialPipe(PipeName, &timeout)
	if err != nil {
		return nil, fmt.Errorf("ncwin service not reachable (%w); is it running, and is this an elevated prompt?", err)
	}
	return conn, nil
}

// Call sends one request to the running service. Streamed output, if any, is
// copied to stream (which may be nil for plain ops).
func Call(op string, args any, stream io.Writer, out any) error {
	conn, err := dial()
	if err != nil {
		return err
	}
	defer conn.Close()
	req := Request{Op: op}
	if args != nil {
		if req.Args, err = json.Marshal(args); err != nil {
			return err
		}
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return err
	}
	dec := json.NewDecoder(conn)
	for {
		var raw map[string]json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return fmt.Errorf("reading response: %w", err)
		}
		if _, done := raw["done"]; !done {
			var c string
			_ = json.Unmarshal(raw["out"], &c)
			if stream != nil {
				_, _ = io.WriteString(stream, c)
			}
			continue
		}
		var resp Response
		b, _ := json.Marshal(raw)
		if err := json.Unmarshal(b, &resp); err != nil {
			return err
		}
		if !resp.OK {
			return fmt.Errorf("%s", resp.Error)
		}
		if out != nil {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	}
}
