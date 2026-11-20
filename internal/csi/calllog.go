package csi

import (
	"context"
	"log"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// logCalls logs every CSI call that changes something (docs/review-history.md P2-7): method, the volume,
// snapshot or node it concerns, duration and result, so failures show in the driver's log and
// not only as Kubernetes events. Frequent read-only calls are logged only when they fail.
// Requests themselves are never logged: they can carry secrets.
func logCalls(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	start := time.Now()
	resp, err := handler(ctx, req)
	method := info.FullMethod[strings.LastIndexByte(info.FullMethod, '/')+1:]
	if err == nil && quietCSIMethod(method) {
		return resp, err
	}
	var b strings.Builder
	b.WriteString("csi ")
	b.WriteString(method)
	for _, f := range callFields(req) {
		b.WriteByte(' ')
		b.WriteString(f)
	}
	b.WriteString(" took=")
	b.WriteString(time.Since(start).Round(time.Millisecond).String())
	if err != nil {
		st := status.Convert(err)
		b.WriteString(" result=")
		b.WriteString(st.Code().String())
		b.WriteString(": ")
		b.WriteString(st.Message())
	} else {
		b.WriteString(" result=OK")
	}
	log.Print(b.String())
	return resp, err
}

// quietCSIMethod lists the calls Kubernetes makes all the time and that change nothing.
func quietCSIMethod(m string) bool {
	switch m {
	case "Probe", "GetPluginInfo", "GetPluginCapabilities", "ControllerGetCapabilities",
		"NodeGetCapabilities", "NodeGetInfo", "GroupControllerGetCapabilities",
		"NodeGetVolumeStats", "ListVolumes", "ListSnapshots", "GetCapacity", "ControllerGetVolume":
		return true
	}
	return false
}

// callFields picks the identifiers a request names, as key=value.
func callFields(req any) []string {
	var out []string
	add := func(k, v string) {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	if r, ok := req.(interface{ GetName() string }); ok {
		add("name", r.GetName())
	}
	if r, ok := req.(interface{ GetVolumeId() string }); ok {
		add("volume", r.GetVolumeId())
	}
	if r, ok := req.(interface{ GetSnapshotId() string }); ok {
		add("snapshot", r.GetSnapshotId())
	}
	if r, ok := req.(interface{ GetSourceVolumeId() string }); ok {
		add("source_volume", r.GetSourceVolumeId())
	}
	if r, ok := req.(interface{ GetGroupSnapshotId() string }); ok {
		add("group_snapshot", r.GetGroupSnapshotId())
	}
	if r, ok := req.(interface{ GetSourceVolumeIds() []string }); ok && len(r.GetSourceVolumeIds()) > 0 {
		add("volumes", strings.Join(r.GetSourceVolumeIds(), ","))
	}
	if r, ok := req.(interface{ GetNodeId() string }); ok {
		add("node", r.GetNodeId())
	}
	return out
}
