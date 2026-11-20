package hardware

import (
	"os"
	"path/filepath"
	"strings"
)

// SPDKInstance identifies the running spdk_tgt process ("<pid>:<start time>", from /proc), or
// returns "" when there isn't exactly one visible (not running, or another PID namespace). A
// different value means the data plane was restarted (docs/review-history.md P2-11): everything velod built
// in SPDK is gone until the reconciler restores it.
func SPDKInstance() string { return spdkInstance("/proc") }

func spdkInstance(procRoot string) string {
	pids := findSPDKProcesses(procRoot)
	if len(pids) != 1 {
		return ""
	}
	stat, err := os.ReadFile(filepath.Join(procRoot, pids[0], "stat"))
	if err != nil {
		return ""
	}
	// Fields after the command name, which is in parentheses and may contain spaces: the
	// process start time (field 22) is the 20th of them.
	s := string(stat)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return ""
	}
	f := strings.Fields(s[i+1:])
	if len(f) < 20 {
		return ""
	}
	return pids[0] + ":" + f[19]
}
