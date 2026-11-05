package backup

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

func TestSkipReasonNamesCloudPlaceholders(t *testing.T) {
	ctx := context.Background()
	counted, detail := skipReason(ctx, &fs.PathError{Op: "read", Path: "a.db-wal", Err: syscall.EDEADLK})
	if counted != "not on this computer" || !strings.Contains(detail, "cloud") || !strings.Contains(detail, "a.db-wal") {
		t.Fatalf("got %q, %q", counted, detail)
	}
	if counted, _ := skipReason(ctx, &fs.PathError{Op: "open", Path: "x", Err: fs.ErrPermission}); counted != "permission denied" {
		t.Fatalf("permission: got %q", counted)
	}
	if counted, _ := skipReason(ctx, errors.New("short read")); counted != "unreadable" {
		t.Fatalf("other: got %q", counted)
	}
}
