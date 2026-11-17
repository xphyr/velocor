package csi

import (
	"net/http"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/velocor/velocor/internal/store"
)

func TestPoolSpaceForProvisioning(t *testing.T) {
	// With space accounting: what admission accepts, not raw free space.
	p := &store.Pool{HealthStatus: store.HealthStatusOnline, FreeBytes: 500 << 30, AvailableBytes: 2 << 30, SpaceState: store.SpaceStateOK}
	if got := poolAvailable(p); got != 2<<30 {
		t.Fatalf("available %d, want 2 GiB (thin volumes make free space meaningless)", got)
	}
	// An older velod without space accounting: fall back to free space.
	old := &store.Pool{HealthStatus: store.HealthStatusOnline, FreeBytes: 500 << 30}
	if got := poolAvailable(old); got != 500<<30 {
		t.Fatalf("available from an older velod %d, want its free space", got)
	}
	for h, want := range map[store.HealthStatus]bool{
		store.HealthStatusOnline: true, store.HealthStatusDegraded: true, store.HealthStatusRebuilding: true,
		store.HealthStatusUnavailable: false, store.HealthStatusFaulted: false, store.HealthStatusReconciling: false,
	} {
		if got := poolServes(&store.Pool{HealthStatus: h}); got != want {
			t.Errorf("%s serves new volumes: %v, want %v", h, got, want)
		}
	}
}

func TestInsufficientStorageIsResourceExhausted(t *testing.T) {
	err := mapAPIError(&ErrAPI{StatusCode: http.StatusInsufficientStorage, Message: "pool full"})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("HTTP 507 maps to %v, want ResourceExhausted", status.Code(err))
	}
}
