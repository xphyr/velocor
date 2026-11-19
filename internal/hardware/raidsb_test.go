package hardware

import (
	"encoding/binary"
	"hash/crc32"
	"testing"
)

// superblock builds a raid1 superblock with two configured members.
func superblock() []byte {
	b := make([]byte, raidSBHeaderBytes+2*raidSBBaseBdevBytes)
	le := binary.LittleEndian
	copy(b, "SPDKRAID")
	le.PutUint16(b[8:], 1)
	le.PutUint32(b[12:], uint32(len(b)))
	copy(b[24:40], []byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff, 0x00})
	copy(b[40:], "raid_pool-0123456789ab")
	le.PutUint64(b[104:], 1000)
	le.PutUint32(b[112:], 512)
	le.PutUint32(b[116:], 1)
	le.PutUint64(b[128:], 7)
	b[136], b[255] = 2, 2
	for i := range 2 {
		m := b[raidSBHeaderBytes+i*raidSBBaseBdevBytes:]
		m[0] = byte(i + 1)
		le.PutUint64(m[16:], 2048)
		le.PutUint64(m[24:], 997952)
		le.PutUint32(m[32:], RaidSBMemberConfigured)
		m[40] = byte(i)
	}
	le.PutUint32(b[16:], ^crc32.Update(0xffffffff, crc32.MakeTable(crc32.Castagnoli), b))
	return b
}

func TestParseRaidSuperblock(t *testing.T) {
	sb, err := ParseRaidSuperblock(superblock())
	if err != nil {
		t.Fatal(err)
	}
	if sb.Name != "raid_pool-0123456789ab" || sb.UUID != "11223344-5566-7788-99aa-bbccddeeff00" || sb.Level != 1 ||
		sb.SeqNumber != 7 || sb.NumMembers != 2 || len(sb.Members) != 2 || sb.Members[1].Slot != 1 ||
		sb.Members[1].State != RaidSBMemberConfigured || sb.RaidBlocks*uint64(sb.BlockSize) != 512000 ||
		sb.Members[1].DataOffset != 2048 || sb.Members[1].DataSize != 997952 {
		t.Fatalf("parsed: %+v", sb)
	}

	bad := superblock()
	bad[100] ^= 1
	if _, err := ParseRaidSuperblock(bad); err == nil {
		t.Fatal("a corrupt superblock must fail its checksum")
	}
	if _, err := ParseRaidSuperblock(superblock()[:300]); err == nil {
		t.Fatal("a truncated superblock must be refused")
	}
	if sb, err := ParseRaidSuperblock(make([]byte, 4096)); sb != nil || err != errNoRaidSB {
		t.Fatalf("blank disk: %+v %v", sb, err)
	}
	huge := superblock()
	binary.LittleEndian.PutUint32(huge[12:], 1<<30)
	if _, err := ParseRaidSuperblock(huge); err == nil {
		t.Fatal("an absurd length must be refused")
	}
}
