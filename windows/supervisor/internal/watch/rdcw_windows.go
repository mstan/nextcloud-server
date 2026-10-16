//go:build windows

package watch

import (
	"context"
	"errors"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Action mirrors FILE_ACTION_* values.
type Action uint32

const (
	Added       Action = 1
	Removed     Action = 2
	Modified    Action = 3
	RenamedFrom Action = 4
	RenamedTo   Action = 5
)

func (a Action) String() string {
	switch a {
	case Added:
		return "added"
	case Removed:
		return "removed"
	case Modified:
		return "modified"
	case RenamedFrom:
		return "renamed-from"
	case RenamedTo:
		return "renamed-to"
	}
	return "unknown"
}

// RawEvent is one FILE_NOTIFY_INFORMATION record; Rel uses '/' separators.
type RawEvent struct {
	Action Action
	Rel    string
}

const notifyFilter = windows.FILE_NOTIFY_CHANGE_FILE_NAME | windows.FILE_NOTIFY_CHANGE_DIR_NAME |
	windows.FILE_NOTIFY_CHANGE_SIZE | windows.FILE_NOTIFY_CHANGE_LAST_WRITE | windows.FILE_NOTIFY_CHANGE_CREATION

// bufSize is 64 KiB: the documented maximum for watches on network shares,
// and plenty locally given the reader goroutine re-arms immediately.
const bufSize = 64 << 10

// watchDir reports changes under root (recursively) until ctx is done.
// overflow is called when the kernel buffer overflowed and events were lost;
// the caller must rescan root.
func watchDir(ctx context.Context, root string, emit func([]RawEvent), overflow func()) error {
	p, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return err
	}
	h, err := windows.CreateFile(p, windows.FILE_LIST_DIRECTORY,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = windows.CancelIoEx(h, nil) })
	defer stop()
	defer windows.CloseHandle(h)

	// DWORD-aligned buffer, as ReadDirectoryChangesW requires.
	buf := make([]uint32, bufSize/4)
	for {
		var n uint32
		err := windows.ReadDirectoryChanges(h, (*byte)(unsafe.Pointer(&buf[0])), bufSize, true, notifyFilter, &n, nil, 0)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil {
			if errors.Is(err, windows.ERROR_NOTIFY_ENUM_DIR) {
				overflow()
				continue
			}
			return err
		}
		if n == 0 {
			overflow()
			continue
		}
		emit(parse(unsafe.Slice((*byte)(unsafe.Pointer(&buf[0])), n)))
	}
}

func parse(b []byte) []RawEvent {
	var out []RawEvent
	for off := 0; off+12 <= len(b); {
		next := *(*uint32)(unsafe.Pointer(&b[off]))
		action := *(*uint32)(unsafe.Pointer(&b[off+4]))
		nameLen := *(*uint32)(unsafe.Pointer(&b[off+8]))
		if off+12+int(nameLen) > len(b) {
			break
		}
		name := unsafe.Slice((*uint16)(unsafe.Pointer(&b[off+12])), nameLen/2)
		rel := windows.UTF16ToString(name)
		out = append(out, RawEvent{Action: Action(action), Rel: toSlash(rel)})
		if next == 0 {
			break
		}
		off += int(next)
	}
	return out
}

func toSlash(s string) string {
	b := []byte(s)
	for i := range b {
		if b[i] == '\\' {
			b[i] = '/'
		}
	}
	return string(b)
}
