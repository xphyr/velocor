package kernelplane

import (
	"slices"
	"testing"

	"github.com/velocor/velocor/internal/kernel"
)

func TestFreshMembers(t *testing.T) {
	mi := func(role int, events uint64, state string) *kernel.MemberInfo {
		return &kernel.MemberInfo{RaidDevices: len(state), Role: role, Events: events, ArrayState: state}
	}
	for _, tc := range []struct {
		name     string
		infos    map[string]*kernel.MemberInfo
		fresh    []string
		complete bool
	}{
		{"healthy", map[string]*kernel.MemberInfo{"a": mi(0, 10, "AA"), "b": mi(1, 10, "AA")}, []string{"a", "b"}, true},
		{"one behind after an unclean stop", map[string]*kernel.MemberInfo{"a": mi(0, 11, "AA"), "b": mi(1, 10, "AA")}, []string{"a", "b"}, true},
		{"a member missing since it was stopped", map[string]*kernel.MemberInfo{"a": mi(0, 10, "AA")}, []string{"a"}, false},
		{"degraded before it was stopped", map[string]*kernel.MemberInfo{"a": mi(0, 20, "A.")}, []string{"a"}, true},
		{"degraded, the failed member back, stale", map[string]*kernel.MemberInfo{"a": mi(0, 20, "A."), "b": mi(1, 10, "AA")}, []string{"a"}, true},
		{"only the stale member", map[string]*kernel.MemberInfo{"b": mi(1, 10, "AA")}, []string{"b"}, false},
		{"the current member missing, an old one present", map[string]*kernel.MemberInfo{"a": mi(0, 30, "AA"), "old": mi(1, 12, "AA")}, []string{"a"}, false},
		{"rebuilding: the target is a fresh spare", map[string]*kernel.MemberInfo{"a": mi(0, 20, "A."), "c": mi(-1, 20, "A.")}, []string{"a", "c"}, true},
		{"raid10 without one member", map[string]*kernel.MemberInfo{"a": mi(0, 5, "A.AA"), "c": mi(2, 5, "A.AA"), "d": mi(3, 5, "A.AA")}, []string{"a", "c", "d"}, true},
		{"raid10, another one missing", map[string]*kernel.MemberInfo{"a": mi(0, 5, "A.AA"), "c": mi(2, 5, "A.AA")}, []string{"a", "c"}, false},
		{"unreadable metadata", map[string]*kernel.MemberInfo{"a": nil}, nil, false},
	} {
		var devs []string
		for d := range tc.infos {
			devs = append(devs, d)
		}
		slices.Sort(devs)
		fresh, complete := freshMembers(devs, tc.infos)
		if !slices.Equal(fresh, tc.fresh) || complete != tc.complete {
			t.Errorf("%s: %v %v, want %v %v", tc.name, fresh, complete, tc.fresh, tc.complete)
		}
	}
}
