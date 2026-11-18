package spdk

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"strings"
)

// NVMe admin commands sent through SPDK (bdev_nvme_send_cmd) to a controller SPDK owns: the
// kernel can't reach a drive bound to vfio-pci, so this is the only way to ask it anything
// SPDK has no RPC for, such as the volatile write cache (TODO.md P0-4).

// NvmeCmd is a 64-byte NVMe submission queue entry. SPDK fills in the command ID and the data
// pointer.
type NvmeCmd struct {
	Opcode uint8
	NSID   uint32
	CDW10  uint32
	CDW11  uint32
}

func (c NvmeCmd) bytes() []byte {
	b := make([]byte, 64)
	b[0] = c.Opcode
	binary.LittleEndian.PutUint32(b[4:], c.NSID)
	binary.LittleEndian.PutUint32(b[40:], c.CDW10)
	binary.LittleEndian.PutUint32(b[44:], c.CDW11)
	return b
}

// NvmeCpl is the completion of an admin command.
type NvmeCpl struct {
	CDW0 uint32 // command-specific result
	SCT  uint8  // status code type
	SC   uint8  // status code
}

// Ok reports whether the command succeeded.
func (c NvmeCpl) Ok() bool { return c.SCT == 0 && c.SC == 0 }

func (c NvmeCpl) String() string { return fmt.Sprintf("status type %#x, code %#x", c.SCT, c.SC) }

// NvmeStatusError is an admin command the controller completed with an error status.
type NvmeStatusError struct {
	Opcode uint8
	Cpl    NvmeCpl
}

func (e *NvmeStatusError) Error() string {
	return fmt.Sprintf("NVMe admin command %#02x failed (%s)", e.Opcode, e.Cpl)
}

// decodeB64 accepts SPDK's URL-safe base64 with or without padding.
func decodeB64(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(s, "="))
}

// BdevNvmeAdminCmd sends an admin command to the NVMe controller ctrl. dataLen > 0 reads that
// many bytes from the controller (controller to host); commands that transfer data to the
// controller aren't needed and not supported. An error status from the controller is returned
// as *NvmeStatusError along with the completion.
func (c *Client) BdevNvmeAdminCmd(ctx context.Context, ctrl string, cmd NvmeCmd, dataLen uint32) (NvmeCpl, []byte, error) {
	params := map[string]any{
		"name":           ctrl,
		"cmd_type":       "admin",
		"data_direction": "c2h",
		"cmdbuf":         base64.RawURLEncoding.EncodeToString(cmd.bytes()),
		"timeout_ms":     5000,
	}
	if dataLen > 0 {
		params["data_len"] = dataLen
	}
	var res struct {
		Cpl  string `json:"cpl"`
		Data string `json:"data"`
	}
	if err := c.Call(ctx, "bdev_nvme_send_cmd", params, &res); err != nil {
		return NvmeCpl{}, nil, err
	}
	raw, err := decodeB64(res.Cpl)
	if err != nil || len(raw) < 16 {
		return NvmeCpl{}, nil, fmt.Errorf("bdev_nvme_send_cmd: bad completion %q", res.Cpl)
	}
	status := binary.LittleEndian.Uint16(raw[14:])
	cpl := NvmeCpl{CDW0: binary.LittleEndian.Uint32(raw[0:]), SC: uint8(status >> 1), SCT: uint8(status>>9) & 0x7}
	var data []byte
	if res.Data != "" {
		if data, err = decodeB64(res.Data); err != nil {
			return cpl, nil, fmt.Errorf("bdev_nvme_send_cmd: bad data: %w", err)
		}
	}
	if !cpl.Ok() {
		return cpl, data, &NvmeStatusError{Opcode: cmd.Opcode, Cpl: cpl}
	}
	return cpl, data, nil
}
