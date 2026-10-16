package ipc

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"
)

// The server must be able to serve its own account repeatedly with no
// AllowSID: each Accept creates a new pipe instance, which needs the server's
// account in the DACL. (In the field the service account was missing and
// every client timed out.)
func TestRepeatedCallsWithoutAllowSID(t *testing.T) {
	pipeName = fmt.Sprintf("%s-test-%d", PipeName, os.Getpid())
	defer func() { pipeName = PipeName }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := Server{Plain: func(_ context.Context, op string, _ json.RawMessage) (any, error) { return op, nil }}
	go srv.Serve(ctx)
	time.Sleep(200 * time.Millisecond)
	for i := 0; i < 3; i++ {
		var got string
		if err := Call("status", nil, nil, &got); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if got != "status" {
			t.Fatalf("call %d returned %q", i, got)
		}
	}
}
