package meta

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestHelperWriteRead(t *testing.T) {
	dev := filepath.Join(t.TempDir(), "dev")
	if err := os.WriteFile(dev, make([]byte, LvolBytes), 0o600); err != nil {
		t.Fatal(err)
	}
	img, _ := EncodeSlot(manifest(3, "p"))
	off := strconv.FormatInt(SlotOffset(1), 10)
	if err := RunHelper([]string{"write", dev, off}, bytes.NewReader(img), nil, false); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := RunHelper([]string{"read", dev, off, strconv.Itoa(SlotBytes)}, nil, &out, false); err != nil {
		t.Fatal(err)
	}
	if out.Len() != SlotBytes || !bytes.Equal(out.Bytes()[:len(img)], img) {
		t.Fatalf("read back %d bytes, mismatch", out.Len())
	}
	if m, ok := DecodeSlot(out.Bytes()); !ok || m.Generation != 3 {
		t.Fatalf("decode: %+v %v", m, ok)
	}
	if err := RunHelper([]string{"write", dev, "100"}, bytes.NewReader(img), nil, false); err == nil {
		t.Fatal("unaligned offset accepted")
	}
	if err := RunHelper([]string{"write", dev, "0"}, bytes.NewReader([]byte("short")), nil, false); err == nil {
		t.Fatal("unaligned length accepted")
	}
}

func TestAlignedBuffer(t *testing.T) {
	for _, n := range []int{HeaderBytes, SlotBytes} {
		if b := aligned(n); len(b) != n || uintptrOf(b)%HeaderBytes != 0 {
			t.Fatalf("aligned(%d): len %d addr %x", n, len(b), uintptrOf(b))
		}
	}
}
