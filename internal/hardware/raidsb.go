package hardware

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"strings"
)

// The SPDK RAID superblock (module/bdev/raid/bdev_raid.h, struct raid_bdev_superblock), at
// the start of every member of a RAID created with a superblock. Little-endian.
const (
	raidSBHeaderBytes   = 256
	raidSBBaseBdevBytes = 64
	raidSBMaxBytes      = raidSBHeaderBytes + 255*raidSBBaseBdevBytes
	raidSBNameBytes     = 64
)

// RAID member states in the superblock (enum raid_bdev_sb_base_bdev_state).
const (
	RaidSBMemberMissing    = 0
	RaidSBMemberConfigured = 1
	RaidSBMemberFailed     = 2
	RaidSBMemberSpare      = 3
)

// RaidSuperblock is the part of SPDK's RAID superblock that pool import needs.
type RaidSuperblock struct {
	UUID       string
	Name       string
	Level      uint32 // SPDK enum raid_level: 0 raid0, 1 raid1, 95 raid5f, 99 concat
	BlockSize  uint32
	RaidBlocks uint64
	SeqNumber  uint64
	NumMembers int
	Members    []RaidSBMember
}

// RaidSBMember is one base bdev slot of the superblock.
type RaidSBMember struct {
	Slot     int
	BdevUUID string
	State    uint32
	// DataOffset and DataSize locate the member's data on its disk, in RAID blocks.
	DataOffset uint64
	DataSize   uint64
}

var errNoRaidSB = errors.New("no SPDK RAID superblock")

// ParseRaidSuperblock decodes and checksums a superblock read from the start of a disk.
func ParseRaidSuperblock(b []byte) (*RaidSuperblock, error) {
	if len(b) < raidSBHeaderBytes || !bytes.Equal(b[:8], []byte("SPDKRAID")) {
		return nil, errNoRaidSB
	}
	le := binary.LittleEndian
	if major := le.Uint16(b[8:]); major != 1 {
		return nil, fmt.Errorf("unsupported SPDK RAID superblock version %d", major)
	}
	length := int(le.Uint32(b[12:]))
	if length < raidSBHeaderBytes || length > raidSBMaxBytes || length > len(b) {
		return nil, fmt.Errorf("SPDK RAID superblock length %d out of range", length)
	}
	sb := append([]byte(nil), b[:length]...)
	want := le.Uint32(sb[16:])
	clear(sb[16:20])
	// spdk_crc32c_update(sb, length, 0): CRC-32C without the usual pre/post inversion.
	if got := ^crc32.Update(0xffffffff, crc32.MakeTable(crc32.Castagnoli), sb); got != want {
		return nil, fmt.Errorf("SPDK RAID superblock checksum mismatch (%08x, want %08x)", got, want)
	}
	s := &RaidSuperblock{
		UUID:       formatUUID(b[24:40]),
		Name:       strings.TrimRight(string(b[40:40+raidSBNameBytes]), "\x00"),
		RaidBlocks: le.Uint64(b[104:]),
		BlockSize:  le.Uint32(b[112:]),
		Level:      le.Uint32(b[116:]),
		SeqNumber:  le.Uint64(b[128:]),
		NumMembers: int(b[136]),
	}
	n := int(b[255])
	if raidSBHeaderBytes+n*raidSBBaseBdevBytes > length {
		return nil, fmt.Errorf("SPDK RAID superblock lists %d members beyond its length", n)
	}
	for i := range n {
		m := b[raidSBHeaderBytes+i*raidSBBaseBdevBytes:]
		s.Members = append(s.Members, RaidSBMember{
			BdevUUID: formatUUID(m[0:16]), DataOffset: le.Uint64(m[16:]), DataSize: le.Uint64(m[24:]),
			State: le.Uint32(m[32:]), Slot: int(m[40]),
		})
	}
	return s, nil
}

// ReadRaidSuperblock reads the superblock from a kernel block device. It returns nil, nil if
// the device carries none.
func ReadRaidSuperblock(dev string) (*RaidSuperblock, error) {
	f, err := os.Open(dev)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, raidSBMaxBytes)
	n, err := io.ReadFull(f, buf)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	sb, err := ParseRaidSuperblock(buf[:n])
	if errors.Is(err, errNoRaidSB) {
		return nil, nil
	}
	return sb, err
}

// formatUUID renders SPDK's struct spdk_uuid (RFC 4122 byte order) as text.
func formatUUID(b []byte) string {
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}
