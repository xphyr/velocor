package spdk

import "context"

// BdevRateLimits represents SPDK QoS limits assigned to a bdev.
type BdevRateLimits struct {
	RwIosPerSec    uint64 `json:"rw_ios_per_sec"`
	RwMbytesPerSec uint64 `json:"rw_mbytes_per_sec"`
	RMbytesPerSec  uint64 `json:"r_mbytes_per_sec"`
	WMbytesPerSec  uint64 `json:"w_mbytes_per_sec"`
}

// BdevInfo describes an SPDK block device returned by bdev_get_bdevs.
type BdevInfo struct {
	Name               string         `json:"name"`
	Aliases            []string       `json:"aliases"`
	ProductName        string         `json:"product_name"`
	BlockSize          uint32         `json:"block_size"`
	NumBlocks          uint64         `json:"num_blocks"`
	UUID               string         `json:"uuid"`
	Claimed            bool           `json:"claimed"`
	AssignedRateLimits BdevRateLimits `json:"assigned_rate_limits"`
	DriverSpecific     map[string]any `json:"driver_specific,omitempty"`
}

// BdevGetBdevs queries SPDK for all bdevs, or a single bdev if name is non-empty.
func (c *Client) BdevGetBdevs(ctx context.Context, name string) ([]BdevInfo, error) {
	var params map[string]any
	if name != "" {
		params = map[string]any{"name": name}
	}
	var res []BdevInfo
	if err := c.Call(ctx, "bdev_get_bdevs", params, &res); err != nil {
		return nil, err
	}
	return res, nil
}

// BdevNvmeAttachController attaches a PCIe or fabric NVMe controller to SPDK.
// Returns the list of created namespace bdev names (e.g., ["Nvme0n1"]).
func (c *Client) BdevNvmeAttachController(ctx context.Context, name, trtype, traddr string) ([]string, error) {
	if trtype == "" {
		trtype = "PCIe"
	}
	params := map[string]any{
		"name":   name,
		"trtype": trtype,
		"traddr": traddr,
	}
	var bdevNames []string
	if err := c.Call(ctx, "bdev_nvme_attach_controller", params, &bdevNames); err != nil {
		return nil, err
	}
	return bdevNames, nil
}

// BdevNvmeDetachController detaches an NVMe controller from SPDK by controller name.
func (c *Client) BdevNvmeDetachController(ctx context.Context, name string) error {
	params := map[string]any{"name": name}
	return c.Call(ctx, "bdev_nvme_detach_controller", params, nil)
}

// NvmeHealthInfo represents S.M.A.R.T. health attributes returned by bdev_nvme_get_controller_health_info.
type NvmeHealthInfo struct {
	CriticalWarning         uint32 `json:"critical_warning"`
	TemperatureCelsius      int32  `json:"temperature_celsius"`
	AvailableSpare          uint32 `json:"available_spare"`
	AvailableSpareThreshold uint32 `json:"available_spare_threshold"`
	PercentageUsed          uint32 `json:"percentage_used"`
	DataUnitsRead           uint64 `json:"data_units_read"`
	DataUnitsWritten        uint64 `json:"data_units_written"`
	HostReadCommands        uint64 `json:"host_read_commands"`
	HostWriteCommands       uint64 `json:"host_write_commands"`
	ControllerBusyTime      uint64 `json:"controller_busy_time"`
	PowerCycles             uint64 `json:"power_cycles"`
	PowerOnHours            uint64 `json:"power_on_hours"`
	UnsafeShutdowns         uint64 `json:"unsafe_shutdowns"`
	MediaErrors             uint64 `json:"media_errors"`
	NumErrLogEntries        uint64 `json:"num_err_log_entries"`
}

// BdevNvmeGetControllerHealthInfo queries S.M.A.R.T. health information for an NVMe controller.
func (c *Client) BdevNvmeGetControllerHealthInfo(ctx context.Context, controllerName string) (*NvmeHealthInfo, error) {
	params := map[string]any{"name": controllerName}
	var res NvmeHealthInfo
	if err := c.Call(ctx, "bdev_nvme_get_controller_health_info", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}

// BdevUringCreate claims a Linux kernel block device (e.g. /dev/sdb) via io_uring.
func (c *Client) BdevUringCreate(ctx context.Context, name, filename string, blockSize uint32) (string, error) {
	params := map[string]any{
		"name":     name,
		"filename": filename,
	}
	if blockSize > 0 {
		params["block_size"] = blockSize
	}
	var bdevName string
	if err := c.Call(ctx, "bdev_uring_create", params, &bdevName); err != nil {
		return "", err
	}
	return bdevName, nil
}

// BdevUringDelete removes an io_uring bdev from SPDK.
func (c *Client) BdevUringDelete(ctx context.Context, name string) error {
	params := map[string]any{"name": name}
	return c.Call(ctx, "bdev_uring_delete", params, nil)
}

// BdevMallocCreate creates an in-memory RAM disk bdev (useful for testing).
func (c *Client) BdevMallocCreate(ctx context.Context, name string, numBlocks uint64, blockSize uint32) (string, error) {
	params := map[string]any{
		"num_blocks": numBlocks,
		"block_size": blockSize,
	}
	if name != "" {
		params["name"] = name
	}
	var bdevName string
	if err := c.Call(ctx, "bdev_malloc_create", params, &bdevName); err != nil {
		return "", err
	}
	return bdevName, nil
}

// BdevSetQoSLimit configures IOPS and bandwidth rate limits on a bdev.
func (c *Client) BdevSetQoSLimit(ctx context.Context, name string, rwIosPerSec, rwMbytesPerSec, rMbytesPerSec, wMbytesPerSec uint64) error {
	params := map[string]any{
		"name":              name,
		"rw_ios_per_sec":    rwIosPerSec,
		"rw_mbytes_per_sec": rwMbytesPerSec,
		"r_mbytes_per_sec":  rMbytesPerSec,
		"w_mbytes_per_sec":  wMbytesPerSec,
	}
	return c.Call(ctx, "bdev_set_qos_limit", params, nil)
}

// BdevCryptoCreate creates a stackable AES-XTS encryption filter bdev over baseBdevName.
func (c *Client) BdevCryptoCreate(ctx context.Context, baseBdevName, name, cryptoPmd, cipher, key, key2 string) (string, error) {
	if cryptoPmd == "" {
		cryptoPmd = "crypto_aesni_mb"
	}
	if cipher == "" {
		cipher = "AES_XTS"
	}
	params := map[string]any{
		"base_bdev_name": baseBdevName,
		"name":           name,
		"crypto_pmd":     cryptoPmd,
		"cipher":         cipher,
		"key":            key,
	}
	if key2 != "" {
		params["key2"] = key2
	}
	var createdName string
	if err := c.Call(ctx, "bdev_crypto_create", params, &createdName); err != nil {
		return "", err
	}
	return createdName, nil
}

// BdevCryptoDelete deletes a stackable encryption filter bdev.
func (c *Client) BdevCryptoDelete(ctx context.Context, name string) error {
	params := map[string]any{"name": name}
	return c.Call(ctx, "bdev_crypto_delete", params, nil)
}

// BdevCompressCreate creates a stackable compression filter bdev over baseBdevName.
func (c *Client) BdevCompressCreate(ctx context.Context, baseBdevName, pmPath string, lbSize uint32) (string, error) {
	params := map[string]any{
		"base_bdev_name": baseBdevName,
		"pm_path":        pmPath,
	}
	if lbSize > 0 {
		params["lb_size"] = lbSize
	}
	var createdName string
	if err := c.Call(ctx, "bdev_compress_create", params, &createdName); err != nil {
		return "", err
	}
	return createdName, nil
}

// BdevCompressDelete deletes a stackable compression filter bdev.
func (c *Client) BdevCompressDelete(ctx context.Context, name string) error {
	params := map[string]any{"name": name}
	return c.Call(ctx, "bdev_compress_delete", params, nil)
}

// BdevIoStat captures per-bdev I/O statistics from bdev_get_iostat.
type BdevIoStat struct {
	Name              string `json:"name"`
	BytesRead         uint64 `json:"bytes_read"`
	NumReadOps        uint64 `json:"num_read_ops"`
	BytesWritten      uint64 `json:"bytes_written"`
	NumWriteOps       uint64 `json:"num_write_ops"`
	BytesUnmapped     uint64 `json:"bytes_unmapped"`
	NumUnmapOps       uint64 `json:"num_unmap_ops"`
	ReadLatencyTicks  uint64 `json:"read_latency_ticks"`
	WriteLatencyTicks uint64 `json:"write_latency_ticks"`
	UnmapLatencyTicks uint64 `json:"unmap_latency_ticks"`
}

// BdevGetIostatResponse is the top-level response returned by bdev_get_iostat.
type BdevGetIostatResponse struct {
	TickRate uint64       `json:"tick_rate"`
	Ticks    uint64       `json:"ticks"`
	Bdevs    []BdevIoStat `json:"bdevs"`
}

// BdevGetIostat queries real-time I/O counters and tick latency for all bdevs or a single bdev.
func (c *Client) BdevGetIostat(ctx context.Context, name string) (*BdevGetIostatResponse, error) {
	var params map[string]any
	if name != "" {
		params = map[string]any{"name": name}
	}
	var res BdevGetIostatResponse
	if err := c.Call(ctx, "bdev_get_iostat", params, &res); err != nil {
		return nil, err
	}
	return &res, nil
}
