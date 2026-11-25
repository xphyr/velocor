package server

import (
	"net/http"
	"strings"
	"testing"
)

// A portal's protocol must be one an export can have: a misspelled one would be saved and then
// never match any export.
func TestPortalProtocolIsChecked(t *testing.T) {
	h := newHarness(t)
	create := func(proto string) (int, map[string]any) {
		body := map[string]any{"name": "p-" + proto, "ip_address": "10.0.0.10"}
		if proto != "" {
			body["protocol"] = proto
		}
		return h.do("POST", "/api/v1/portals", h.admin, body)
	}
	for _, proto := range []string{"NVME_TCP", "nvmeof_tcp", "UNSPECIFIED", "iSCSI", "FC"} {
		code, out := create(proto)
		msg, _ := out["error"].(string)
		if code != http.StatusBadRequest || !strings.Contains(msg, proto) {
			t.Errorf("%q: %d %v, want 400 naming it", proto, code, out)
		}
	}
	if left := h.expect(h.do("GET", "/api/v1/portals", h.admin, nil))(http.StatusOK)["portals"]; left != nil && len(left.([]any)) != 0 {
		t.Fatalf("refused portals were saved: %v", left)
	}
	for proto, want := range map[string]struct {
		proto string
		port  float64
	}{"": {"NVMEOF_TCP", 4420}, "NVMEOF_RDMA": {"NVMEOF_RDMA", 4420}, "ISCSI": {"ISCSI", 3260}} {
		pt := h.expect(create(proto))(http.StatusCreated)["portal"].(map[string]any)
		if pt["protocol"] != want.proto || pt["port"] != want.port {
			t.Errorf("%q: got %v, want %v", proto, pt, want)
		}
	}
}
