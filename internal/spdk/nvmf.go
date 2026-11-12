package spdk

import "context"

// NvmfTransport describes an SPDK NVMe-oF transport layer (TCP / RDMA).
type NvmfTransport struct {
	Trtype              string `json:"trtype"`
	MaxQueueDepth       uint32 `json:"max_queue_depth"`
	MaxIoQpairsPerCtrlr uint32 `json:"max_io_qpairs_per_ctrlr"`
	InCapsuleDataSize   uint32 `json:"in_capsule_data_size"`
	MaxIoSize           uint32 `json:"max_io_size"`
	IoUnitSize          uint32 `json:"io_unit_size"`
	MaxAqDepth          uint32 `json:"max_aq_depth"`
	NumSharedBuffers    uint32 `json:"num_shared_buffers"`
	BufCacheSize        uint32 `json:"buf_cache_size"`
}

// NvmfListenAddress represents a listener endpoint attached to an NVMe-oF subsystem.
type NvmfListenAddress struct {
	Trtype  string `json:"trtype"`
	Adrfam  string `json:"adrfam"`
	Traddr  string `json:"traddr"`
	Trsvcid string `json:"trsvcid"`
}

// NvmfHost represents an authorized Host NQN on an NVMe-oF subsystem.
type NvmfHost struct {
	Nqn string `json:"nqn"`
}

// NvmfNamespace describes a namespace attached to an NVMe-oF subsystem.
type NvmfNamespace struct {
	Nsid     uint32 `json:"nsid"`
	BdevName string `json:"bdev_name"`
	Nguid    string `json:"nguid"`
	Eui64    string `json:"eui64"`
	UUID     string `json:"uuid"`
	Anagrpid uint32 `json:"anagrpid,omitempty"`
}

// NvmfSubsystem describes an SPDK NVMe-oF target subsystem.
type NvmfSubsystem struct {
	Nqn             string              `json:"nqn"`
	Subtype         string              `json:"subtype"`
	ListenAddresses []NvmfListenAddress `json:"listen_addresses"`
	AllowAnyHost    bool                `json:"allow_any_host"`
	Hosts           []NvmfHost          `json:"hosts"`
	SerialNumber    string              `json:"serial_number"`
	ModelNumber     string              `json:"model_number"`
	MaxNamespaces   uint32              `json:"max_namespaces"`
	MinCntlid       uint32              `json:"min_cntlid"`
	MaxCntlid       uint32              `json:"max_cntlid"`
	Namespaces      []NvmfNamespace     `json:"namespaces"`
}

// NvmfGetTransports queries SPDK for initialized NVMe-oF transports.
func (c *Client) NvmfGetTransports(ctx context.Context) ([]NvmfTransport, error) {
	var res []NvmfTransport
	if err := c.Call(ctx, "nvmf_get_transports", nil, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// NvmfCreateTransport initializes an NVMe-oF transport ("TCP" or "RDMA") in spdk_tgt.
func (c *Client) NvmfCreateTransport(ctx context.Context, trtype string) error {
	params := map[string]any{
		"trtype":             trtype,
		"num_shared_buffers": 512,
		"buf_cache_size":     32,
	}
	return c.Call(ctx, "nvmf_create_transport", params, nil)
}

// NvmfGetSubsystems returns all NVMe-oF subsystems configured in spdk_tgt.
func (c *Client) NvmfGetSubsystems(ctx context.Context) ([]NvmfSubsystem, error) {
	var res []NvmfSubsystem
	if err := c.Call(ctx, "nvmf_get_subsystems", nil, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// NvmfCreateSubsystem creates a new NVMe-oF subsystem with ANA reporting enabled.
func (c *Client) NvmfCreateSubsystem(ctx context.Context, nqn, serialNumber, modelNumber string, allowAnyHost bool, maxNamespaces uint32) error {
	if modelNumber == "" {
		modelNumber = "Velocor All-Flash Controller"
	}
	if maxNamespaces == 0 {
		maxNamespaces = 32
	}
	params := map[string]any{
		"nqn":            nqn,
		"serial_number":  serialNumber,
		"model_number":   modelNumber,
		"allow_any_host": allowAnyHost,
		"max_namespaces": maxNamespaces,
		"ana_reporting":  true,
	}
	return c.Call(ctx, "nvmf_create_subsystem", params, nil)
}

// NvmfDeleteSubsystem deletes an NVMe-oF subsystem by NQN.
func (c *Client) NvmfDeleteSubsystem(ctx context.Context, nqn string) error {
	params := map[string]any{"nqn": nqn}
	return c.Call(ctx, "nvmf_delete_subsystem", params, nil)
}

// NvmfSubsystemAddNs attaches a bdev as a namespace on an NVMe-oF subsystem and returns the assigned NSID.
func (c *Client) NvmfSubsystemAddNs(ctx context.Context, nqn, bdevName string, nsid uint32, uuid string) (uint32, error) {
	ns := map[string]any{
		"bdev_name": bdevName,
	}
	if nsid > 0 {
		ns["nsid"] = nsid
	}
	if uuid != "" {
		ns["uuid"] = uuid
	}
	params := map[string]any{
		"nqn":       nqn,
		"namespace": ns,
	}
	var assignedNSID uint32
	if err := c.Call(ctx, "nvmf_subsystem_add_ns", params, &assignedNSID); err != nil {
		return 0, err
	}
	return assignedNSID, nil
}

// NvmfSubsystemRemoveNs removes a namespace from an NVMe-oF subsystem.
func (c *Client) NvmfSubsystemRemoveNs(ctx context.Context, nqn string, nsid uint32) error {
	params := map[string]any{
		"nqn":  nqn,
		"nsid": nsid,
	}
	return c.Call(ctx, "nvmf_subsystem_remove_ns", params, nil)
}

// NvmfSubsystemAddListener adds an IP/Port listener to an NVMe-oF subsystem.
func (c *Client) NvmfSubsystemAddListener(ctx context.Context, nqn, trtype, adrfam, traddr, trsvcid string) error {
	if adrfam == "" {
		adrfam = "IPv4"
	}
	params := map[string]any{
		"nqn": nqn,
		"listen_address": map[string]any{
			"trtype":  trtype,
			"adrfam":  adrfam,
			"traddr":  traddr,
			"trsvcid": trsvcid,
		},
	}
	return c.Call(ctx, "nvmf_subsystem_add_listener", params, nil)
}

// NvmfSubsystemRemoveListener removes an IP/Port listener from an NVMe-oF subsystem.
func (c *Client) NvmfSubsystemRemoveListener(ctx context.Context, nqn, trtype, adrfam, traddr, trsvcid string) error {
	if adrfam == "" {
		adrfam = "IPv4"
	}
	params := map[string]any{
		"nqn": nqn,
		"listen_address": map[string]any{
			"trtype":  trtype,
			"adrfam":  adrfam,
			"traddr":  traddr,
			"trsvcid": trsvcid,
		},
	}
	return c.Call(ctx, "nvmf_subsystem_remove_listener", params, nil)
}

// NvmfSubsystemAddHost authorizes a Host NQN to connect to an NVMe-oF subsystem.
func (c *Client) NvmfSubsystemAddHost(ctx context.Context, nqn, hostNqn string) error {
	params := map[string]any{
		"nqn":  nqn,
		"host": hostNqn,
	}
	return c.Call(ctx, "nvmf_subsystem_add_host", params, nil)
}

// NvmfSubsystemRemoveHost revokes a Host NQN from an NVMe-oF subsystem.
func (c *Client) NvmfSubsystemRemoveHost(ctx context.Context, nqn, hostNqn string) error {
	params := map[string]any{
		"nqn":  nqn,
		"host": hostNqn,
	}
	return c.Call(ctx, "nvmf_subsystem_remove_host", params, nil)
}

// NvmfSubsystemListenerSetANAState sets the Asymmetric Namespace Access state
// ("optimized", "non_optimized", "inaccessible") on an NVMe-oF listener.
func (c *Client) NvmfSubsystemListenerSetANAState(ctx context.Context, nqn, trtype, adrfam, traddr, trsvcid, anaState string) error {
	if adrfam == "" {
		adrfam = "IPv4"
	}
	params := map[string]any{
		"nqn": nqn,
		"listen_address": map[string]any{
			"trtype":  trtype,
			"adrfam":  adrfam,
			"traddr":  traddr,
			"trsvcid": trsvcid,
		},
		"ana_state": anaState,
	}
	return c.Call(ctx, "nvmf_subsystem_listener_set_ana_state", params, nil)
}
