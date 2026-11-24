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
		"nqn.2024-01.x:../../subsystems/evil":                             false,
		"nqn.2024-01.x:a b":                                               false,
		"nqn.2024-01.x:a\n":                                               false,
	} {
		if err := ValidateNQN(nqn); (err == nil) != ok {
			t.Errorf("ValidateNQN(%q) = %v, want ok=%v", nqn, err, ok)
		}
	}
}

func TestValidateIQN(t *testing.T) {
	for iqn, ok := range map[string]bool{
		"iqn.1994-05.com.redhat:3c5f89a2b1":            true,
		"iqn.2026-11.io.velocor:csi-vol-aaa-ede24dfd8": true,
		"iqn.2026-11.io.velocor:VM_1":                  true,
		"eui.02004567A425678D":                         true,
		"naa.52004567BA64678D":                         true,
		"iqn.2026-11.io.velocor:../../core/iblock_0/x": false,
		"iqn.2026-11.io.velocor:a/b":                   false,
		"iqn.2026-11.io.velocor:a b":                   false,
		"iqn.20x6-11.io.velocor:a":                     false,
		"eui.02004567A425678":                          false,
		"../../x":                                      false,
		"":                                             false,
	} {
		if err := ValidateIQN(iqn); (err == nil) != ok {
			t.Errorf("ValidateIQN(%q) = %v, want ok=%v", iqn, err, ok)
		}
	}
}
