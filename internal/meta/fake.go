package meta

import (
	"context"
	"fmt"
	"sync"
)

// MemDevice is an in-memory Device for tests: one SlotBytes*2 buffer per lvol.
type MemDevice struct {
	mu     sync.Mutex
	lvols  map[string][]byte
	Writes int
	Fail   error // returned by every call while set
}

func (m *MemDevice) buf(lvol string) []byte {
	if m.lvols == nil {
		m.lvols = map[string][]byte{}
	}
	b, ok := m.lvols[lvol]
	if !ok {
		b = make([]byte, LvolBytes)
		m.lvols[lvol] = b
	}
	return b
}

// WriteSlot implements Device.
func (m *MemDevice) WriteSlot(_ context.Context, lvol string, slot int, img []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return m.Fail
	}
	if slot < 0 || slot > 1 || len(img) > SlotBytes {
		return fmt.Errorf("bad slot write: slot %d, %d bytes", slot, len(img))
	}
	copy(m.buf(lvol)[SlotOffset(slot):], img)
	m.Writes++
	return nil
}

// ReadSlots implements Device.
func (m *MemDevice) ReadSlots(_ context.Context, lvol string) ([][]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Fail != nil {
		return nil, m.Fail
	}
	b := m.buf(lvol)
	return [][]byte{append([]byte(nil), b[:SlotBytes]...), append([]byte(nil), b[SlotBytes:]...)}, nil
}

// Corrupt overwrites part of a slot (tests of torn writes).
func (m *MemDevice) Corrupt(lvol string, slot int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	copy(m.buf(lvol)[SlotOffset(slot)+HeaderBytes:], []byte("torn write"))
}
