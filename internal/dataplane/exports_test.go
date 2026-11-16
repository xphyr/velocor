package dataplane

import "testing"

func TestValidateNQN(t *testing.T) {
	for nqn, ok := range map[string]bool{
		"nqn.2014-08.org.nvmexpress:uuid:0b4d2c3e-1111-4222-8333-444455556666": true,
		"nqn.2026-01.io.velocor:node:worker-1":                                 true,
		"nqn.2014-08.org.nvmexpress:uuid:worker-01":                            false, // SPDK rejects: not a UUID
		"nqn.host-a":                           false,
		"iqn.1994-05.com.redhat:host":          false,
		"nqn.2014-08.org.nvmexpress.discovery": false,
		"nqn.20x4-08.org.example:x":            false,
		"nqn.2014-08.org.nvmexpress:uuid:0b4d2c3e11114222833344445555666": false,
	} {
		if err := ValidateNQN(nqn); (err == nil) != ok {
			t.Errorf("ValidateNQN(%q) = %v, want ok=%v", nqn, err, ok)
		}
	}
}
