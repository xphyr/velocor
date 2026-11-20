package csi

import (
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// velod refuses a group snapshot of volumes in use with 412 (docs/review-history.md P1-15): Kubernetes must see
// FailedPrecondition (retry once the volumes are free), not AlreadyExists or Internal.
func TestGroupSnapshotRefusalIsFailedPrecondition(t *testing.T) {
	for code, want := range map[int]codes.Code{412: codes.FailedPrecondition, 409: codes.AlreadyExists, 404: codes.NotFound, 500: codes.Internal} {
		if got := status.Code(mapGroupControllerError(&ErrAPI{StatusCode: code, Message: "x"})); got != want {
			t.Errorf("HTTP %d -> %s, want %s", code, got, want)
		}
	}
}
