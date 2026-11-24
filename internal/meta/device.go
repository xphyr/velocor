package meta

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"syscall"
)

// Device reads and writes the slots of a pool's metadata volume.
type Device interface {
	// WriteSlot writes a slot image (block aligned) to slot 0 or 1 of the volume.
	WriteSlot(ctx context.Context, volumeUUID string, slot int, img []byte) error
	// ReadSlots returns both slot images.
	ReadSlots(ctx context.Context, volumeUUID string) ([][]byte, error)
}

var errHelperStuck = errors.New("metadata I/O helper did not finish")

// RunHelper is "velod meta-io": direct I/O on the metadata device, outside velod's process.
//
//	meta-io write <dev> <offset>          (data on stdin, a multiple of 4096 bytes)
//	meta-io read  <dev> <offset> <length> (data on stdout)
//
// directIO is false only in tests (tmpfs has no O_DIRECT).
func RunHelper(args []string, stdin io.Reader, stdout io.Writer, directIO bool) error {
	if len(args) < 3 {
		return errors.New("usage: meta-io write <dev> <offset> | read <dev> <offset> <length>")
	}
	op, dev := args[0], args[1]
	off, err := strconv.ParseInt(args[2], 10, 64)
	if err != nil || off%HeaderBytes != 0 {
		return fmt.Errorf("offset %q must be a multiple of %d", args[2], HeaderBytes)
	}
	flags := os.O_RDONLY
	if op == "write" {
		flags = os.O_WRONLY
	}
	if directIO {
		flags |= syscall.O_DIRECT
	}
	f, err := os.OpenFile(dev, flags, 0)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	switch op {
	case "write":
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		if len(data) == 0 || len(data)%HeaderBytes != 0 {
			return fmt.Errorf("write of %d bytes is not a multiple of %d", len(data), HeaderBytes)
		}
		buf := aligned(len(data))
		copy(buf, data)
		if _, err := f.WriteAt(buf, off); err != nil {
			return err
		}
		return f.Sync()
	case "read":
		if len(args) < 4 {
			return errors.New("read needs a length")
		}
		n, err := strconv.Atoi(args[3])
		if err != nil || n%HeaderBytes != 0 {
			return fmt.Errorf("length %q must be a multiple of %d", args[3], HeaderBytes)
		}
		buf := aligned(n)
		if _, err := f.ReadAt(buf, off); err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		_, err = stdout.Write(buf)
		return err
	}
	return fmt.Errorf("unknown operation %q", op)
}

// aligned returns a zeroed buffer whose address is aligned for direct I/O.
func aligned(n int) []byte {
	b := make([]byte, n+HeaderBytes)
	off := 0
	if r := int(uintptrOf(b) % HeaderBytes); r != 0 {
		off = HeaderBytes - r
	}
	return b[off : off+n]
}
