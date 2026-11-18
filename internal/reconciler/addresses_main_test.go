package reconciler

import (
	"os"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane"
)

// The portals in these tests use addresses this machine doesn't have: treat every address
// as the appliance's, except TEST-NET-3 (203.0.113.0/24), which stands for one that's gone.
func TestMain(m *testing.M) {
	dataplane.SetAddressCheck(func(ip string) bool { return !strings.HasPrefix(ip, "203.0.113.") })
	os.Exit(m.Run())
}
