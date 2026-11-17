package meta

import "unsafe"

// uintptrOf returns the address of a slice's first byte (for direct-I/O alignment).
func uintptrOf(b []byte) uintptr {
	return uintptr(unsafe.Pointer(unsafe.SliceData(b)))
}
