package meta

import (
	"context"
	"fmt"
	"sync"
)

// MemDevice is an in-memory Device for tests: one SlotBytes*2 buffer per volume.
type MemDevice struct {
	mu      sync.Mutex
	volumes map[string][]byte
	Writes  int
	Fail    error // returned by every call while set
}

func (m *MemDevice) buf(volume string) []byte {
	if m.volumes == nil {
		m.volumes = map[string][]byte{}
	}
	b, ok := m.volumes[volume]
	if !ok {
		b = make([]byte, VolumeBytes)
		m.volumes[volume] = b
	}
	return b
}

// WriteSlot implements Device.
func (m *MemDevice) WriteSlot(_ context.Context, volume string, slot int, img []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if slot < 0 || slot > 1 || len(img) > SlotBytes {
		return fmt.Errorf("bad slot write: slot %d, %d bytes", slot, len(img))
	}
	copy(m.buf(volume)[SlotOffset(slot):], img)
	m.Writes++
	return nil
}

// ReadSlots implements Device.
func (m *MemDevice) ReadSlots(_ context.Context, volume string) ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	b := m.buf(volume)
	return [][]byte{append([]byte(nil), b[:SlotBytes]...), append([]byte(nil), b[SlotBytes:]...)}, nil
}

// Corrupt overwrites part of a slot (tests of torn writes).
func (m *MemDevice) Corrupt(volume string, slot int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	copy(m.buf(volume)[SlotOffset(slot)+HeaderBytes:], []byte("torn write"))
}
