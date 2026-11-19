package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

)

// NetworkInterface describes a host network interface and its active IP/NetworkManager state.
type NetworkInterface struct {
	Name       string   `json:"name"`
	Type       string   `json:"type,omitempty"`
	State      string   `json:"state"`
	MAC        string   `json:"mac,omitempty"`
	MTU        int      `json:"mtu"`
	Method     string   `json:"method,omitempty"`
	Addresses  []string `json:"addresses"`
	Gateway    string   `json:"gateway,omitempty"`
	DNS        []string `json:"dns,omitempty"`
	Connection string   `json:"connection,omitempty"`
	LinkUp     bool     `json:"link_up"`
}

// NetworkStatus summarizes host network interfaces and NetworkManager availability.
type NetworkStatus struct {
	Available      bool               `json:"available"`
	NMCLIAvailable bool               `json:"nmcli_available"`
	Message        string             `json:"message,omitempty"`
	Hostname       string             `json:"hostname,omitempty"`
	Interfaces     []NetworkInterface `json:"interfaces"`
}

// UpdateNetworkInterfaceRequest configures IPv4 and MTU settings for a host interface via nmcli.
type UpdateNetworkInterfaceRequest struct {
	Method  string `json:"method"`            // "dhcp" | "manual" | "disabled"
	Address string `json:"address,omitempty"` // CIDR e.g. "10.10.10.20/24"
	Gateway string `json:"gateway,omitempty"`
	DNS     string `json:"dns,omitempty"` // comma-separated IP list
	MTU     int    `json:"mtu,omitempty"`
	// ConfirmTimeoutSeconds is how long the change waits for confirmation before it is rolled
	// back (60 to 600; 0 means 120).
	ConfirmTimeoutSeconds int `json:"confirm_timeout_seconds,omitempty"`
	// Force allows changing an interface that carries portals (it interrupts their hosts).
	Force bool `json:"force,omitempty"`
}

var validIfaceNameRe = regexp.MustCompile(`^[a-zA-Z0-9._:-]{1,32}$`)

type ipAddrEntry struct {
	IfName    string `json:"ifname"`
	LinkType  string `json:"link_type"`
	OperState string `json:"operstate"`
	Address   string `json:"address"`
	MTU       int    `json:"mtu"`
	AddrInfo  []struct {
		Family    string `json:"family"`
		Local     string `json:"local"`
		PrefixLen int    `json:"prefixlen"`
	} `json:"addr_info"`
}

func isIgnoredVirtualIface(name string) bool {
	if name == "lo" {
		return true
	}
	for _, prefix := range []string{"veth", "cni-", "podman", "docker", "virbr", "tunl"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

func validateNetworkRequest(name string, req UpdateNetworkInterfaceRequest) ([]string, error) {
	if !validIfaceNameRe.MatchString(name) || name == "lo" {
		return nil, fmt.Errorf("invalid or protected interface name %q", name)
	}
	method := strings.ToLower(strings.TrimSpace(req.Method))
	if method != "dhcp" && method != "manual" && method != "disabled" {
		return nil, fmt.Errorf("method must be 'dhcp', 'manual', or 'disabled'")
	}
	if req.MTU != 0 && (req.MTU < 576 || req.MTU > 9216) {
		return nil, fmt.Errorf("mtu must be between 576 and 9216")
	}

	var dnsList []string
	if strings.TrimSpace(req.DNS) != "" {
		for _, part := range strings.Split(req.DNS, ",") {
			ipStr := strings.TrimSpace(part)
			if ipStr == "" {
				continue
			}
			if net.ParseIP(ipStr) == nil {
				return nil, fmt.Errorf("invalid DNS IP address %q", ipStr)
			}
			dnsList = append(dnsList, ipStr)
		}
	}

	if method == "manual" {
		addr := strings.TrimSpace(req.Address)
		if addr == "" {
			return nil, fmt.Errorf("address (CIDR, e.g. 10.10.10.20/24) is required for manual method")
		}
		if _, _, err := net.ParseCIDR(addr); err != nil {
			return nil, fmt.Errorf("invalid CIDR address %q: %w", addr, err)
		}
		gw := strings.TrimSpace(req.Gateway)
		if gw != "" && net.ParseIP(gw) == nil {
			return nil, fmt.Errorf("invalid gateway IP address %q", gw)
		}
	}

	return dnsList, nil
}

func splitEscapedNMCLI(line string) []string {
	// nmcli -t escapes literal colons with '\:'.
	var fields []string
	var cur strings.Builder
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == ':' {
			fields = append(fields, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteByte(ch)
	}
	fields = append(fields, cur.String())
	return fields
}

func queryNetworkStatus(ctx context.Context) NetworkStatus {
	hostname, _ := os.Hostname()
	status := NetworkStatus{
		Hostname:   hostname,
		Interfaces: []NetworkInterface{},
	}

	out, err := hostCommand(ctx, "ip", "-j", "addr", "show").CombinedOutput()
	if err != nil {
		status.Available = false
		status.Message = fmt.Sprintf("unable to query host interfaces via ip: %v", err)
		return status
	}

	var entries []ipAddrEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		status.Available = false
		status.Message = fmt.Sprintf("unable to parse ip output: %v", err)
		return status
	}
	status.Available = true

	ifaceMap := make(map[string]*NetworkInterface)
	var order []string
	for _, e := range entries {
		if isIgnoredVirtualIface(e.IfName) {
			continue
		}
		ni := &NetworkInterface{
			Name:      e.IfName,
			Type:      e.LinkType,
			State:     strings.ToUpper(e.OperState),
			MAC:       e.Address,
			MTU:       e.MTU,
			Addresses: []string{},
			LinkUp:    strings.EqualFold(e.OperState, "UP"),
		}
		for _, a := range e.AddrInfo {
			if a.Family == "inet" && a.Local != "" {
				ni.Addresses = append(ni.Addresses, fmt.Sprintf("%s/%d", a.Local, a.PrefixLen))
			}
		}
		ifaceMap[e.IfName] = ni
		order = append(order, e.IfName)
	}

	nmOut, nmErr := hostCommand(ctx, "nmcli", "-t", "-f", "DEVICE,TYPE,STATE,CONNECTION", "device", "status").CombinedOutput()
	if nmErr == nil {
		status.NMCLIAvailable = true
		for _, line := range strings.Split(strings.TrimSpace(string(nmOut)), "\n") {
			if strings.TrimSpace(line) == "" {
				continue
			}
			cols := splitEscapedNMCLI(line)
			if len(cols) < 4 {
				continue
			}
			dev, devType, devState, conName := cols[0], cols[1], cols[2], cols[3]
			ni, ok := ifaceMap[dev]
			if !ok {
				continue
			}
			if devType != "" {
				ni.Type = devType
			}
			if conName != "" && conName != "--" {
				ni.Connection = conName
				enrichInterfaceFromNMCLI(ctx, ni, conName)
			} else if strings.Contains(strings.ToLower(devState), "disconnected") {
				ni.Method = "disabled"
			}
		}
	} else {
		status.Message = "NetworkManager (nmcli) not detected; interface listing is read-only"
	}

	for _, name := range order {
		status.Interfaces = append(status.Interfaces, *ifaceMap[name])
	}
	return status
}

func enrichInterfaceFromNMCLI(ctx context.Context, ni *NetworkInterface, conName string) {
	out, err := hostCommand(ctx, "nmcli", "-t", "-f", "ipv4.method,ipv4.gateway,ipv4.dns,connection.autoconnect", "connection", "show", conName).CombinedOutput()
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(out), "\n") {
		idx := strings.IndexByte(line, ':')
		if idx <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:idx])
		val := strings.TrimSpace(line[idx+1:])
		switch key {
		case "ipv4.method":
			switch val {
			case "auto":
				ni.Method = "dhcp"
			case "manual":
				ni.Method = "manual"
			case "disabled":
				ni.Method = "disabled"
			default:
				ni.Method = val
			}
		case "ipv4.gateway":
			if val != "" && val != "--" {
				ni.Gateway = val
			}
		case "ipv4.dns":
			if val != "" && val != "--" {
				for _, d := range strings.Split(val, ",") {
					d = strings.TrimSpace(d)
					if d != "" {
						ni.DNS = append(ni.DNS, d)
					}
				}
			}
		}
	}
}

func findNMCLIConnectionForDevice(ctx context.Context, dev string) (string, error) {
	out, err := hostCommand(ctx, "nmcli", "-t", "-f", "NAME,DEVICE", "connection", "show").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("nmcli connection show failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		cols := splitEscapedNMCLI(line)
		if len(cols) >= 2 && cols[1] == dev && cols[0] != "" {
			return cols[0], nil
		}
	}
	return "", nil
}

// applyNetworkInterfaceConfig configures name with nmcli. temporary changes only the running
// configuration (a new profile isn't saved, a modification uses --temporary); the saved one
// changes when the change is confirmed (applied again with temporary false).
func applyNetworkInterfaceConfig(ctx context.Context, name string, req UpdateNetworkInterfaceRequest, dnsList []string, temporary bool) error {
	conName, err := findNMCLIConnectionForDevice(ctx, name)
	if err != nil {
		return err
	}
	if conName == "" {
		conName = name
		save := "yes"
		if temporary {
			save = "no"
		}
		out, err := hostCommand(ctx, "nmcli", "connection", "add", "save", save, "type", "ethernet", "ifname", name, "con-name", conName).CombinedOutput()
		if err != nil {
			return fmt.Errorf("nmcli connection add failed: %w: %s", err, strings.TrimSpace(string(out)))
		}
	}

	method := strings.ToLower(strings.TrimSpace(req.Method))
	args := []string{"connection", "modify"}
	if temporary {
		args = append(args, "--temporary")
	}
	args = append(args, conName)
	switch method {
	case "disabled":
		args = append(args, "connection.autoconnect", "no", "ipv4.method", "disabled", "ipv4.addresses", "", "ipv4.gateway", "", "ipv4.dns", "")
	case "dhcp":
		args = append(args, "connection.autoconnect", "yes", "ipv4.method", "auto", "ipv4.addresses", "", "ipv4.gateway", "")
		if len(dnsList) > 0 {
			args = append(args, "ipv4.dns", strings.Join(dnsList, ","))
		} else {
			args = append(args, "ipv4.dns", "")
		}
	case "manual":
		args = append(args,
			"connection.autoconnect", "yes",
			"ipv4.method", "manual",
			"ipv4.addresses", strings.TrimSpace(req.Address),
			"ipv4.gateway", strings.TrimSpace(req.Gateway),
			"ipv4.dns", strings.Join(dnsList, ","),
		)
	}
	if req.MTU > 0 {
		args = append(args, "802-3-ethernet.mtu", fmt.Sprintf("%d", req.MTU))
	}

	if out, err := hostCommand(ctx, "nmcli", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connection modify failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if !temporary {
		return nil // confirming: the running configuration already is this one
	}

	if method == "disabled" {
		if out, err := hostCommand(ctx, "nmcli", "device", "disconnect", name).CombinedOutput(); err != nil {
			// Ignore error if already disconnected.
			_ = out
		}
		return nil
	}

	if out, err := hostCommand(ctx, "nmcli", "connection", "up", conName).CombinedOutput(); err != nil {
		return fmt.Errorf("nmcli connection up failed: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (s *Server) handleGetNetwork(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	st := s.queryNetwork(ctx)
	pending, last := s.net.snapshot()
	writeJSON(w, http.StatusOK, map[string]any{
		"available": st.Available, "nmcli_available": st.NMCLIAvailable, "message": st.Message,
		"hostname": st.Hostname, "interfaces": st.Interfaces, "pending_change": pending, "last_change": last,
	})
}

func (s *Server) queryNetwork(ctx context.Context) NetworkStatus {
	if s.netQuery != nil {
		return s.netQuery(ctx)
	}
	return queryNetworkStatus(ctx)
}

// nmHost is the real nmBackend: checkpoints through NetworkManager's D-Bus API (busctl on the
// host), settings through nmcli.
type nmHost struct{}

const (
	nmService = "org.freedesktop.NetworkManager"
	nmPath    = "/org/freedesktop/NetworkManager"
	// NM_CHECKPOINT_CREATE_FLAG_DELETE_NEW_CONNECTIONS: a rollback also removes a profile
	// the change created.
	nmCheckpointDeleteNew = 0x02
)

func busctl(ctx context.Context, args ...string) (string, error) {
	out, err := hostCommand(ctx, "busctl", append([]string{"--system"}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("busctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// objectPath parses busctl's `o "/path"` reply.
func objectPath(reply string) (string, error) {
	f := strings.Fields(reply)
	if len(f) != 2 || f[0] != "o" {
		return "", fmt.Errorf("unexpected reply %q", reply)
	}
	return strings.Trim(f[1], `"`), nil
}

func (nmHost) Checkpoint(ctx context.Context, iface string, timeout time.Duration) (string, error) {
	reply, err := busctl(ctx, "call", nmService, nmPath, nmService, "GetDeviceByIpIface", "s", iface)
	if err != nil {
		return "", err
	}
	dev, err := objectPath(reply)
	if err != nil {
		return "", err
	}
	reply, err = busctl(ctx, "call", nmService, nmPath, nmService, "CheckpointCreate", "aouu", "1", dev,
		fmt.Sprint(int(timeout.Seconds())), fmt.Sprint(nmCheckpointDeleteNew))
	if err != nil {
		return "", err
	}
	return objectPath(reply)
}

func (nmHost) CheckpointExists(ctx context.Context, cp string) (bool, error) {
	reply, err := busctl(ctx, "get-property", nmService, nmPath, nmService, "Checkpoints")
	if err != nil {
		return false, err
	}
	for _, f := range strings.Fields(reply) {
		if strings.Trim(f, `"`) == cp {
			return true, nil
		}
	}
	return false, nil
}

func (nmHost) Commit(ctx context.Context, cp string) error {
	_, err := busctl(ctx, "call", nmService, nmPath, nmService, "CheckpointDestroy", "o", cp)
	return err
}

func (nmHost) Rollback(ctx context.Context, cp string) error {
	_, err := busctl(ctx, "call", nmService, nmPath, nmService, "CheckpointRollback", "o", cp)
	return err
}

func (nmHost) Apply(ctx context.Context, iface string, req UpdateNetworkInterfaceRequest, dns []string, temporary bool) error {
	return applyNetworkInterfaceConfig(ctx, iface, req, dns, temporary)
}
