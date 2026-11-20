package csi

import (
	"bytes"
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestLogCalls(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	call := func(method string, req any, err error) string {
		buf.Reset()
		_, _ = logCalls(context.Background(), req, &grpc.UnaryServerInfo{FullMethod: "/csi.v1.Node/" + method},
			func(context.Context, any) (any, error) { return nil, err })
		return buf.String()
	}
	got := call("NodeStageVolume", &csi.NodeStageVolumeRequest{VolumeId: "vol-1", Secrets: map[string]string{"pw": "s3cret"}},
		status.Error(codes.Internal, "connect target: iscsiadm login failed"))
	for _, want := range []string{"csi NodeStageVolume", "volume=vol-1", "result=Internal: connect target: iscsiadm login failed"} {
		if !strings.Contains(got, want) {
			t.Errorf("log %q lacks %q", got, want)
		}
	}
	if strings.Contains(got, "s3cret") {
		t.Fatalf("a secret reached the log: %q", got)
	}
	if got := call("ControllerPublishVolume", &csi.ControllerPublishVolumeRequest{VolumeId: "vol-1", NodeId: "node-a"}, nil); !strings.Contains(got, "volume=vol-1 node=node-a") || !strings.Contains(got, "result=OK") {
		t.Errorf("publish: %q", got)
	}
	if got := call("NodeGetVolumeStats", &csi.NodeGetVolumeStatsRequest{VolumeId: "vol-1"}, nil); got != "" {
		t.Errorf("a successful frequent read-only call was logged: %q", got)
	}
	if got := call("NodeGetVolumeStats", &csi.NodeGetVolumeStatsRequest{VolumeId: "vol-1"}, status.Error(codes.NotFound, "gone")); !strings.Contains(got, "result=NotFound") {
		t.Errorf("a failed read-only call must be logged: %q", got)
	}
}
