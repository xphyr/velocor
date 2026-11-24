package meta

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// BlockDevice reaches the metadata volume as its block device (an LV). The I/O runs in a helper
// process ("velod meta-io") with O_DIRECT, so a hung device blocks only the helper.
type BlockDevice struct {
	// Path returns the device of the metadata volume with this UUID.
	Path    func(ctx context.Context, lvolUUID string) (string, error)
	Helper  string // path of the velod binary
	Timeout time.Duration
}

func (b *BlockDevice) helper(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	timeout := b.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.Helper, append([]string{"meta-io"}, args...)...)
	cmd.Stdin = bytes.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, fmt.Errorf("%w within %s", errHelperStuck, timeout)
	}
	if err != nil {
		return nil, fmt.Errorf("metadata I/O: %v: %s", err, bytes.TrimSpace(errb.Bytes()))
	}
	return out.Bytes(), nil
}

// WriteSlot implements Device.
func (b *BlockDevice) WriteSlot(ctx context.Context, lvol string, slot int, img []byte) error {
	dev, err := b.Path(ctx, lvol)
	if err != nil {
		return err
	}
	_, err = b.helper(ctx, img, "write", dev, strconv.FormatInt(SlotOffset(slot), 10))
	return err
}

// ReadSlots implements Device.
func (b *BlockDevice) ReadSlots(ctx context.Context, lvol string) ([][]byte, error) {
	dev, err := b.Path(ctx, lvol)
	if err != nil {
		return nil, err
	}
	var slots [][]byte
	for i := range 2 {
		s, err := b.helper(ctx, nil, "read", dev, strconv.FormatInt(SlotOffset(i), 10), strconv.Itoa(SlotBytes))
		if err != nil {
			return nil, err
		}
		slots = append(slots, s)
	}
	return slots, nil
}
