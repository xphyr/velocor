package dataplane

import (
	"errors"
	"fmt"
	"net"
	"sync"

	"github.com/velocor/velocor/internal/store"
)

var (
	addrMu    sync.RWMutex
	addrCheck = interfaceHasAddress
)

// AddressPresent reports whether ip can be listened on here: one of the appliance's interface
// addresses, or a wildcard. SPDK refuses a listener on any other address with a bare
// "Invalid parameters" and logs four errors per attempt.
func AddressPresent(ip string) bool {
	addrMu.RLock()
	defer addrMu.RUnlock()
	return addrCheck(ip)
}

// SetAddressCheck replaces the address check (tests: the portals in unit tests use addresses
// the test machine doesn't have). It returns a function that restores the previous one.
func SetAddressCheck(check func(ip string) bool) (restore func()) {
	addrMu.Lock()
	defer addrMu.Unlock()
	prev := addrCheck
	addrCheck = check
	return func() {
		addrMu.Lock()
		defer addrMu.Unlock()
		addrCheck = prev
	}
}

func interfaceHasAddress(ip string) bool {
	want := net.ParseIP(ip)
	if want == nil {
		return false
	}
	if want.IsUnspecified() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return true // can't tell: let SPDK decide
	}
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && n.IP.Equal(want) {
			return true
		}
	}
	return false
}

// AddressMissingError says a portal can't be served because its address isn't on the
// appliance. The export is served on its other portals meanwhile.
type AddressMissingError struct{ Portal *store.Portal }

func (e *AddressMissingError) Error() string {
	return fmt.Sprintf("portal %s (%s) has address %s, which isn't on this appliance; add the address or give the export another portal",
		e.Portal.Name, e.Portal.ID, e.Portal.IPAddress)
}

func (e *AddressMissingError) Unwrap() error { return store.ErrInvalidArgument }

func errAddressMissing(p *store.Portal) error { return &AddressMissingError{Portal: p} }

// missingPortals returns the portals err reports as missing their address, and whether
// that is all err says.
func missingPortals(err error) (ids []string, only bool) {
	var errs []error
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	} else {
		errs = []error{err}
	}
	only = len(errs) > 0
	for _, e := range errs {
		var m *AddressMissingError
		if errors.As(e, &m) {
			ids = append(ids, m.Portal.ID)
		} else {
			only = false
		}
	}
	return ids, only
}
