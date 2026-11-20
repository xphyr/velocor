package csi

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	"github.com/velocor/velocor/internal/store"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// paramPortals is the StorageClass parameter naming the portals a class's volumes are exported
// on (docs/review-history.md P1-13): Velocor portal names or IDs, comma-separated, all of the class's
// protocol. Nodes connect to every one (NVMe/TCP multipath; iSCSI fails over between them).
// Without it the driver's --default-portals apply, and without those the protocol's only
// portal; with several portals and no choice, provisioning is refused rather than guessed.
const paramPortals = "portals"

// SetDefaultPortals sets the portals used for classes without a "portals" parameter.
func (s *ControllerServer) SetDefaultPortals(spec string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultPortals = strings.TrimSpace(spec)
}

// checkPortals validates a CreateVolume's portal choice against the appliance's portals. No
// portal at all for the protocol is left to publish, which reports it (as before).
func (s *ControllerServer) checkPortals(ctx context.Context, params map[string]string) error {
	proto := normalizeProtocol(params["protocol"])
	portals, err := s.client.ListPortals(ctx)
	if err != nil {
		return mapAPIError(err)
	}
	spec := strings.TrimSpace(params[paramPortals])
	if spec == "" && s.defaultFor(proto, portals) == "" && countProtocol(portals, proto) == 0 {
		return nil
	}
	if _, err := s.resolvePortals(spec, proto, portals); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	return nil
}

// resolvePortals turns a volume's portal list (from its StorageClass; empty for none) into
// portals of protocol proto, in the listed order.
func (s *ControllerServer) resolvePortals(spec string, proto store.ExportProtocol, portals []*store.Portal) ([]*store.Portal, error) {
	spec = strings.TrimSpace(spec)
	fromClass := spec != ""
	if !fromClass {
		spec = s.defaultFor(proto, portals)
	}
	var chosen []*store.Portal
	if spec == "" {
		for _, p := range portals {
			if p.Protocol == proto {
				chosen = append(chosen, p)
			}
		}
		switch len(chosen) {
		case 0:
			return nil, fmt.Errorf("no %s portal is configured in Velocor", proto)
		case 1:
		default:
			return nil, fmt.Errorf("%d %s portals are configured in Velocor (%s); set the StorageClass parameter %q (or the driver's defaultPortals) to the ones Kubernetes nodes should use",
				len(chosen), proto, portalNames(chosen), paramPortals)
		}
	} else {
		seen := map[string]bool{}
		for _, ref := range strings.Split(spec, ",") {
			ref = strings.TrimSpace(ref)
			if ref == "" {
				continue
			}
			p := findPortal(portals, ref)
			if p == nil {
				return nil, fmt.Errorf("portal %q (parameter %q) doesn't exist in Velocor; portals: %s", ref, paramPortals, portalNames(portals))
			}
			if p.Protocol != proto {
				if !fromClass {
					continue // the driver default may list both protocols
				}
				return nil, fmt.Errorf("portal %q is %s, but the StorageClass uses %s", ref, p.Protocol, proto)
			}
			if !seen[p.ID] {
				seen[p.ID] = true
				chosen = append(chosen, p)
			}
		}
		if len(chosen) == 0 {
			return nil, fmt.Errorf("parameter %q names no %s portal", paramPortals, proto)
		}
	}
	if os.Getenv("VELOCOR_PORTAL_OVERRIDE") == "" {
		for _, p := range chosen {
			if ip := net.ParseIP(p.IPAddress); ip != nil && ip.IsUnspecified() {
				return nil, fmt.Errorf("portal %q listens on every address (%s), which nodes can't connect to; create a portal for each SAN address and list those in %q",
					p.Name, p.IPAddress, paramPortals)
			}
		}
	}
	return chosen, nil
}

// defaultFor returns the driver's default portal list if it names a portal of proto.
func (s *ControllerServer) defaultFor(proto store.ExportProtocol, portals []*store.Portal) string {
	if s.defaultPortals == "" {
		return ""
	}
	for _, ref := range strings.Split(s.defaultPortals, ",") {
		if ref = strings.TrimSpace(ref); ref == "" {
			continue
		}
		// A portal that doesn't exist is resolvePortals' to report.
		if p := findPortal(portals, ref); p == nil || p.Protocol == proto {
			return s.defaultPortals
		}
	}
	return ""
}

func findPortal(portals []*store.Portal, ref string) *store.Portal {
	for _, p := range portals {
		if p.ID == ref {
			return p
		}
	}
	for _, p := range portals {
		if p.Name == ref {
			return p
		}
	}
	return nil
}

func countProtocol(portals []*store.Portal, proto store.ExportProtocol) int {
	n := 0
	for _, p := range portals {
		if p.Protocol == proto {
			n++
		}
	}
	return n
}

func portalNames(portals []*store.Portal) string {
	if len(portals) == 0 {
		return "none"
	}
	names := make([]string, len(portals))
	for i, p := range portals {
		names[i] = fmt.Sprintf("%s (%s %s)", p.Name, p.Protocol, net.JoinHostPort(p.IPAddress, fmt.Sprint(p.Port)))
	}
	return strings.Join(names, ", ")
}
