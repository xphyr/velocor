// Package spdktest provides a stateful in-process fake of the spdk_tgt JSON-RPC
// server for tests.
//
// The fake distinguishes between *media* state (what is persisted on the disks:
// RAID superblocks, lvstores, lvols) and *runtime* state (bdevs, RAID bdevs,
// loaded lvstores, NVMe-oF and iSCSI targets). Restart() discards runtime state
// exactly like an spdk_tgt restart with no saved config, which is what Velocor
// relies on its reconciler to rebuild.
//
// It also models the examine hazard that makes naive reconciliation dangerous:
// when a RAID bdev appears, its lvstore is only loaded once examine completes
// (bdev_wait_for_examine, or immediately when LazyExamine is false). Calling
// bdev_lvol_create_lvstore on a bdev whose existing lvstore has not been loaded
// yet *overwrites* that lvstore, just as real SPDK would.
package spdktest

import (
	"bufio"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/velocor/velocor/internal/spdk"
)

// Error codes mirroring what spdk_tgt returns.
const (
	codeInvalidParams = -32602
	codeENODEV        = -19
	codeEEXIST        = -17
	codeEBUSY         = -16
)

type rpcErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func errf(code int, format string, a ...any) *rpcErr {
	return &rpcErr{Code: code, Message: fmt.Sprintf(format, a...)}
}

// mediaLvol is an lvol persisted inside an lvstore.
type mediaLvol struct {
	UUID       string
	Name       string
	SizeMiB    uint64
	Thin       bool
	IsSnapshot bool
	Parent     string // parent snapshot UUID (clone relationship)
	Allocated  uint64
	Clusters   map[uint64]bool // clusters owned by this blob (not its ancestors)
}

// mediaLvstore is an lvstore persisted on a (RAID) bdev's backing media.
type mediaLvstore struct {
	UUID        string
	Name        string
	ClusterSize uint32
	TotalClust  uint64
	Lvols       map[string]*mediaLvol // by UUID
	Generation  int                   // bumped each time the lvstore is (re)initialised
}

type subsystem struct {
	spdk.NvmfSubsystem
}

// Fake is a fake spdk_tgt.
type Fake struct {
	t        testing.TB
	sockPath string
	ln       net.Listener

	mu sync.Mutex

	// Configuration knobs.
	LazyExamine bool // when true, lvstores on new RAID bdevs load only on bdev_wait_for_examine

	// Physical media present in the machine. key = PCI address or filename.
	nvmeMedia  map[string]bool
	fileMedia  map[string]bool
	superblock map[string]*raidSuperblock // media key -> superblock
	lvsMedia   map[string]*mediaLvstore   // raid media key -> persisted lvstore

	// Runtime state.
	baseBdevs    map[string]string // bdev name -> media key
	bdevUUID     map[string]string // base bdev name -> bdev UUID
	nvmeCtrls    map[string]string // controller name -> PCI address
	raids        map[string]*raidBdev
	pendingLoad  map[string]bool   // raid media key with lvstore not yet examined
	loadedLvs    map[string]string // lvstore name -> raid media key
	qos          map[string]spdk.BdevRateLimits
	transports   map[string]bool
	subsystems   map[string]*subsystem
	portalGroups map[int][]spdk.IscsiPortal
	initGroups   map[int]spdk.IscsiInitiatorGroup
	targetNodes  map[string]*spdk.IscsiTargetNode
	nvmeConns    map[string][]spdk.NvmfController // subsystem NQN -> connected hosts
	failures     map[string]int                   // method -> remaining failures (<0 = forever)
	lateFailures map[string]int                   // method -> calls that succeed in SPDK but report an error
	calls        []string
	uuidCounter  int

	rebuildStep    uint32 // percent a rebuild advances per bdev_raid_get_bdevs (0: manual)
	rebuildMaxMBps uint32
}

// New starts a fake spdk_tgt listening on a UNIX socket in t.TempDir().
func New(t testing.TB) *Fake {
	t.Helper()
	f := &Fake{
		t:            t,
		sockPath:     filepath.Join(t.TempDir(), "spdk.sock"),
		nvmeMedia:    map[string]bool{},
		fileMedia:    map[string]bool{},
		superblock:   map[string]*raidSuperblock{},
		lvsMedia:     map[string]*mediaLvstore{},
		failures:     map[string]int{},
		lateFailures: map[string]int{},
	}
	f.resetRuntime()
	ln, err := net.Listen("unix", f.sockPath)
	if err != nil {
		t.Fatalf("spdktest: listen: %v", err)
	}
	f.ln = ln
	t.Cleanup(func() { _ = ln.Close() })
	go f.serve()
	return f
}

// Client returns an SPDK client connected to the fake.
func (f *Fake) Client() *spdk.Client { return spdk.NewClient(f.sockPath) }

// AddNVMe registers a physical NVMe drive at the given PCI address.
func (f *Fake) AddNVMe(pci string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nvmeMedia[pci] = true
}

// AddFile registers a physical kernel block device path usable with bdev_uring.
func (f *Fake) AddFile(path string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileMedia[path] = true
}

// RemoveMedia simulates a failed / pulled drive.
func (f *Fake) RemoveMedia(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.nvmeMedia, key)
	delete(f.fileMedia, key)
}

// Restart simulates spdk_tgt restarting with no saved configuration.
func (f *Fake) Restart() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resetRuntime()
}

// Fail makes the next n calls to method fail (n < 0: fail forever, n == 0: clear).
func (f *Fake) Fail(method string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n == 0 {
		delete(f.failures, method)
		return
	}
	f.failures[method] = n
}

// FailAfterApplying makes the next n calls to method take effect in the fake but report an
// error, like a client that timed out while spdk_tgt went on and completed the request.
func (f *Fake) FailAfterApplying(method string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lateFailures[method] = n
}

// Calls returns the methods invoked so far.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

// CallCount returns how many times method was invoked.
func (f *Fake) CallCount(method string) int {
	n := 0
	for _, c := range f.Calls() {
		if c == method {
			n++
		}
	}
	return n
}

// LvstoreGeneration returns how many times the lvstore persisted on the media of the
// RAID named raidName has been initialised (1 = never wiped). 0 if none.
func (f *Fake) LvstoreGeneration(lvsName string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, lvs := range f.lvsMedia {
		if lvs.Name == lvsName {
			return lvs.Generation
		}
	}
	return 0
}

// LvolExists reports whether an lvol with the given lvs/name alias exists on media.
func (f *Fake) LvolExists(alias string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, l := f.findLvolLocked(alias)
	return l != nil
}

// Subsystem returns a copy of a live NVMe-oF subsystem.
func (f *Fake) Subsystem(nqn string) (spdk.NvmfSubsystem, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.subsystems[nqn]
	if !ok {
		return spdk.NvmfSubsystem{}, false
	}
	return s.NvmfSubsystem, true
}

// TargetNode returns a copy of a live iSCSI target node.
func (f *Fake) TargetNode(name string) (spdk.IscsiTargetNode, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, ok := f.targetNodes[name]
	if !ok {
		return spdk.IscsiTargetNode{}, false
	}
	return *n, true
}

// InitiatorGroup returns a copy of a live iSCSI initiator group.
func (f *Fake) InitiatorGroup(tag int) (spdk.IscsiInitiatorGroup, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ig, ok := f.initGroups[tag]
	return ig, ok
}

// QoS returns the rate limits configured on a bdev.
func (f *Fake) QoS(bdev string) spdk.BdevRateLimits {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.qos[f.canonicalBdevLocked(bdev)]
}

// WriteCluster simulates a host write that allocates cluster idx in the live lvol ref.
func (f *Fake) WriteCluster(ref string, idx uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, l := f.findLvolLocked(ref)
	if l == nil {
		f.t.Fatalf("spdktest: WriteCluster: lvol %s not found", ref)
	}
	if l.Clusters == nil {
		l.Clusters = map[uint64]bool{}
	}
	if !l.Clusters[idx] {
		l.Clusters[idx] = true
		l.Allocated++
	}
}

// LvolSizeMiB returns the size of the lvol with alias or UUID ref.
func (f *Fake) LvolSizeMiB(ref string) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, l := f.findLvolLocked(ref)
	if l == nil {
		return 0
	}
	return l.SizeMiB
}

func (f *Fake) resetRuntime() {
	f.baseBdevs = map[string]string{}
	f.bdevUUID = map[string]string{}
	f.nvmeCtrls = map[string]string{}
	f.raids = map[string]*raidBdev{}
	f.pendingLoad = map[string]bool{}
	f.loadedLvs = map[string]string{}
	f.qos = map[string]spdk.BdevRateLimits{}
	f.transports = map[string]bool{}
	f.subsystems = map[string]*subsystem{}
	f.portalGroups = map[int][]spdk.IscsiPortal{}
	f.initGroups = map[int]spdk.IscsiInitiatorGroup{}
	f.targetNodes = map[string]*spdk.IscsiTargetNode{}
	f.nvmeConns = map[string][]spdk.NvmfController{}
}

// ConnectHost simulates hostNQN connecting to an NVMe-oF subsystem.
func (f *Fake) ConnectHost(nqn, hostNQN string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nvmeConns[nqn] = append(f.nvmeConns[nqn], spdk.NvmfController{Cntlid: uint32(len(f.nvmeConns[nqn]) + 1), HostNQN: hostNQN, NumIOQpairs: 4})
}

func (f *Fake) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			var req struct {
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
				ID     uint64          `json:"id"`
			}
			if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
				return
			}
			res, rerr := f.dispatch(req.Method, req.Params)
			resp := map[string]any{"jsonrpc": "2.0", "id": req.ID}
			if rerr != nil {
				resp["error"] = rerr
			} else {
				resp["result"] = res
			}
			_ = json.NewEncoder(c).Encode(resp)
		}(conn)
	}
}

func (f *Fake) newUUID() string {
	f.uuidCounter++
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", f.uuidCounter)
}

func (f *Fake) dispatch(method string, raw json.RawMessage) (any, *rpcErr) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, method)
	if n, ok := f.failures[method]; ok {
		if n > 0 {
			n--
			if n == 0 {
				delete(f.failures, method)
			} else {
				f.failures[method] = n
			}
		}
		return nil, errf(-5, "injected failure for %s", method)
	}
	var p map[string]any
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, errf(codeInvalidParams, "bad params: %v", err)
		}
	}
	if p == nil {
		p = map[string]any{}
	}
	// AIO bdevs behave exactly like io_uring ones for the fake's purposes.
	alias := map[string]string{"bdev_aio_create": "bdev_uring_create", "bdev_aio_delete": "bdev_uring_delete"}
	if a, ok := alias[method]; ok {
		method = a
	}
	h, ok := handlers[method]
	if !ok {
		return nil, errf(-32601, "Method not found")
	}
	res, rerr := h(f, p)
	if n := f.lateFailures[method]; n > 0 && rerr == nil {
		f.lateFailures[method] = n - 1
		return nil, errf(-5, "injected failure after applying %s", method)
	}
	return res, rerr
}

// ---------- param helpers ----------

func str(p map[string]any, k string) string {
	v, _ := p[k].(string)
	return v
}

func num(p map[string]any, k string) uint64 {
	v, _ := p[k].(float64)
	return uint64(v)
}

func boolean(p map[string]any, k string) bool {
	v, _ := p[k].(bool)
	return v
}

func strs(p map[string]any, k string) []string {
	arr, _ := p[k].([]any)
	out := make([]string, 0, len(arr))
	for _, v := range arr {
		s, _ := v.(string)
		out = append(out, s)
	}
	return out
}

func remarshal(v any, dst any) {
	b, _ := json.Marshal(v)
	_ = json.Unmarshal(b, dst)
}

// ---------- bdev lookups ----------

func (f *Fake) findLvolLocked(ref string) (*mediaLvstore, *mediaLvol) {
	for key, lvsName := range f.loadedLvsByKey() {
		lvs := f.lvsMedia[key]
		if lvs == nil {
			continue
		}
		for _, l := range lvs.Lvols {
			if l.UUID == ref || lvsName+"/"+l.Name == ref {
				return lvs, l
			}
		}
	}
	return nil, nil
}

func (f *Fake) loadedLvsByKey() map[string]string {
	out := map[string]string{}
	for name, key := range f.loadedLvs {
		out[key] = name
	}
	return out
}

func (f *Fake) bdevExistsLocked(name string) bool {
	if _, ok := f.baseBdevs[name]; ok {
		return true
	}
	if r, ok := f.raids[name]; ok && f.raidOnline(r) {
		return true
	}
	_, l := f.findLvolLocked(name)
	return l != nil
}

func (f *Fake) canonicalBdevLocked(name string) string {
	if _, l := f.findLvolLocked(name); l != nil {
		return l.UUID
	}
	return name
}

// examineLocked loads lvstores for online RAID bdevs whose load is pending.
func (f *Fake) examineLocked() {
	for key := range f.pendingLoad {
		var raid *raidBdev
		for _, r := range f.raids {
			if r.MediaKey == key {
				raid = r
			}
		}
		if raid == nil || !f.raidOnline(raid) {
			continue
		}
		if lvs := f.lvsMedia[key]; lvs != nil {
			f.loadedLvs[lvs.Name] = key
		}
		delete(f.pendingLoad, key)
	}
}

// raidAppearedLocked is called when a RAID bdev becomes online.
func (f *Fake) raidAppearedLocked(r *raidBdev) {
	if f.lvsMedia[r.MediaKey] == nil {
		return
	}
	f.pendingLoad[r.MediaKey] = true
	if !f.LazyExamine {
		f.examineLocked()
	}
}

// ---------- handlers ----------

type handler func(f *Fake, p map[string]any) (any, *rpcErr)

var handlers = map[string]handler{
	"spdk_get_version": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		return map[string]any{"version": "SPDK v26.09 (fake)"}, nil
	},
	"bdev_wait_for_examine": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		f.examineLocked()
		return true, nil
	},
	"bdev_get_bdevs": func(f *Fake, p map[string]any) (any, *rpcErr) {
		var out []spdk.BdevInfo
		for name := range f.baseBdevs {
			out = append(out, spdk.BdevInfo{Name: name, BlockSize: 512, Claimed: f.memberOfRaid(name)})
		}
		for _, r := range f.raids {
			if f.raidOnline(r) {
				out = append(out, spdk.BdevInfo{Name: r.Name, UUID: r.UUID, BlockSize: 512, ProductName: "Raid Volume"})
			}
		}
		for key, lvsName := range f.loadedLvsByKey() {
			for _, l := range f.lvsMedia[key].Lvols {
				out = append(out, spdk.BdevInfo{
					Name: l.UUID, UUID: l.UUID, Aliases: []string{lvsName + "/" + l.Name},
					BlockSize: 512, NumBlocks: l.SizeMiB * 2048, ProductName: "Logical Volume",
					AssignedRateLimits: f.qos[l.UUID],
				})
			}
		}
		if name := str(p, "name"); name != "" {
			var filtered []spdk.BdevInfo
			for _, b := range out {
				if b.Name == name || (len(b.Aliases) > 0 && b.Aliases[0] == name) {
					filtered = append(filtered, b)
				}
			}
			if len(filtered) == 0 {
				return nil, errf(codeENODEV, "No such device")
			}
			return filtered, nil
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	},
	"bdev_nvme_attach_controller": func(f *Fake, p map[string]any) (any, *rpcErr) {
		name, traddr := str(p, "name"), str(p, "traddr")
		if !f.nvmeMedia[traddr] {
			return nil, errf(codeENODEV, "No such device")
		}
		if _, ok := f.nvmeCtrls[name]; ok {
			return nil, errf(codeEEXIST, "controller %s already exists", name)
		}
		f.nvmeCtrls[name] = traddr
		bdev := name + "n1"
		f.baseBdevs[bdev] = traddr
		f.bdevUUID[bdev] = "nvme-ns-" + traddr // NVMe namespaces report a stable UUID
		f.assembleFromSuperblocksLocked(bdev)
		return []string{bdev}, nil
	},
	"bdev_nvme_detach_controller": func(f *Fake, p map[string]any) (any, *rpcErr) {
		name := str(p, "name")
		if _, ok := f.nvmeCtrls[name]; !ok {
			return nil, errf(codeENODEV, "No such device")
		}
		delete(f.nvmeCtrls, name)
		f.baseBdevRemovedLocked(name + "n1")
		delete(f.baseBdevs, name+"n1")
		return true, nil
	},
	"bdev_nvme_get_controller_health_info": func(f *Fake, p map[string]any) (any, *rpcErr) {
		if _, ok := f.nvmeCtrls[str(p, "name")]; !ok {
			return nil, errf(codeENODEV, "No such device")
		}
		return spdk.NvmeHealthInfo{TemperatureCelsius: 35, AvailableSpare: 100}, nil
	},
	"bdev_uring_create": func(f *Fake, p map[string]any) (any, *rpcErr) {
		name, file := str(p, "name"), str(p, "filename")
		if !f.fileMedia[file] {
			return nil, errf(codeENODEV, "No such device")
		}
		if _, ok := f.baseBdevs[name]; ok {
			return nil, errf(codeEEXIST, "bdev %s already exists", name)
		}
		f.baseBdevs[name] = file
		f.bdevUUID[name] = str(p, "uuid")
		if f.bdevUUID[name] == "" {
			f.bdevUUID[name] = f.newUUID() // like SPDK: random unless given
		}
		f.assembleFromSuperblocksLocked(name)
		return name, nil
	},
	"bdev_uring_delete": func(f *Fake, p map[string]any) (any, *rpcErr) {
		name := str(p, "name")
		if _, ok := f.baseBdevs[name]; !ok {
			return nil, errf(codeENODEV, "No such device")
		}
		f.baseBdevRemovedLocked(name)
		delete(f.baseBdevs, name)
		return true, nil
	},
	"bdev_raid_delete": func(f *Fake, p map[string]any) (any, *rpcErr) {
		name := str(p, "name")
		r, ok := f.raids[name]
		if !ok {
			return nil, errf(codeENODEV, "No such device")
		}
		if r.deleting {
			return nil, errf(-32603, "Operation already in progress")
		}
		for _, key := range f.loadedLvs {
			if key == r.MediaKey {
				// As observed on spdk_tgt v26.09: the RAID is deconfigured but can't finish
				// while the lvstore holds it; the request never completes (the client times
				// out) and the RAID stays "offline" until the lvstore is deleted.
				r.deleting, r.deleteClearSB = true, boolean(p, "clear_sb")
				return nil, errf(-110, "bdev_raid_delete did not complete: %s is still claimed", name)
			}
		}
		f.finishRaidDelete(name, boolean(p, "clear_sb"))
		return true, nil
	},
	"bdev_lvol_get_lvstores": func(f *Fake, p map[string]any) (any, *rpcErr) {
		var out []spdk.LvolStoreInfo
		for name, key := range f.loadedLvs {
			lvs := f.lvsMedia[key]
			if q := str(p, "lvs_name"); q != "" && q != name {
				continue
			}
			if q := str(p, "uuid"); q != "" && q != lvs.UUID {
				continue
			}
			var used uint64
			for _, l := range lvs.Lvols {
				used += l.Allocated
			}
			base := ""
			for _, r := range f.raids {
				if r.MediaKey == key {
					base = r.Name
				}
			}
			out = append(out, spdk.LvolStoreInfo{
				UUID: lvs.UUID, Name: name, BaseBdev: base, ClusterSize: lvs.ClusterSize,
				TotalDataClusters: lvs.TotalClust, FreeClusters: lvs.TotalClust - used, BlockSize: 512,
			})
		}
		if (str(p, "lvs_name") != "" || str(p, "uuid") != "") && len(out) == 0 {
			return nil, errf(codeENODEV, "No such device")
		}
		return out, nil
	},
	"bdev_lvol_create_lvstore": func(f *Fake, p map[string]any) (any, *rpcErr) {
		bdev, name := str(p, "bdev_name"), str(p, "lvs_name")
		r, ok := f.raids[bdev]
		if !ok || !f.raidOnline(r) {
			return nil, errf(codeENODEV, "bdev %s not found", bdev)
		}
		if _, loaded := f.loadedLvs[name]; loaded {
			return nil, errf(codeEEXIST, "lvstore %s already exists", name)
		}
		for _, key := range f.loadedLvs {
			if key == r.MediaKey {
				return nil, errf(codeEBUSY, "bdev %s already claimed", bdev)
			}
		}
		// Not yet examined (or no lvstore at all): this initialises the blobstore and
		// destroys whatever was on the media.
		gen := 1
		if old := f.lvsMedia[r.MediaKey]; old != nil {
			gen = old.Generation + 1
		}
		cs := uint32(num(p, "cluster_sz"))
		if cs == 0 {
			cs = 4 << 20
		}
		lvs := &mediaLvstore{UUID: f.newUUID(), Name: name, ClusterSize: cs, TotalClust: 1 << 20, Lvols: map[string]*mediaLvol{}, Generation: gen}
		f.lvsMedia[r.MediaKey] = lvs
		delete(f.pendingLoad, r.MediaKey)
		f.loadedLvs[name] = r.MediaKey
		return lvs.UUID, nil
	},
	"bdev_lvol_delete_lvstore": func(f *Fake, p map[string]any) (any, *rpcErr) {
		for name, key := range f.loadedLvs {
			lvs := f.lvsMedia[key]
			if name == str(p, "lvs_name") || lvs.UUID == str(p, "uuid") {
				delete(f.loadedLvs, name)
				delete(f.lvsMedia, key)
				for rn, r := range f.raids {
					if r.deleting && r.MediaKey == key {
						f.finishRaidDelete(rn, r.deleteClearSB)
					}
				}
				return true, nil
			}
		}
		return nil, errf(codeENODEV, "No such device")
	},
	"bdev_lvol_create": func(f *Fake, p map[string]any) (any, *rpcErr) {
		key, ok := f.loadedLvs[str(p, "lvs_name")]
		if !ok {
			return nil, errf(codeENODEV, "lvstore %s not found", str(p, "lvs_name"))
		}
		lvs := f.lvsMedia[key]
		name := str(p, "lvol_name")
		if len(name) > spdk.MaxLvolNameLen {
			return nil, errf(codeInvalidParams, "name too long")
		}
		for _, l := range lvs.Lvols {
			if l.Name == name {
				return nil, errf(codeEEXIST, "lvol %s already exists", name)
			}
		}
		l := &mediaLvol{UUID: f.newUUID(), Name: name, SizeMiB: num(p, "size_in_mib"), Thin: boolean(p, "thin_provision")}
		lvs.Lvols[l.UUID] = l
		return l.UUID, nil
	},
	"bdev_lvol_resize": func(f *Fake, p map[string]any) (any, *rpcErr) {
		_, l := f.findLvolLocked(str(p, "name"))
		if l == nil {
			return nil, errf(codeENODEV, "No such device")
		}
		l.SizeMiB = num(p, "size_in_mib")
		return true, nil
	},
	"bdev_lvol_delete": func(f *Fake, p map[string]any) (any, *rpcErr) {
		lvs, l := f.findLvolLocked(str(p, "name"))
		if l == nil {
			return nil, errf(codeENODEV, "No such device")
		}
		var clones []*mediaLvol
		for _, o := range lvs.Lvols {
			if o.Parent == l.UUID {
				clones = append(clones, o)
			}
		}
		if len(clones) > 1 {
			return nil, errf(codeEBUSY, "snapshot %s has more than one clone", l.Name)
		}
		for _, c := range clones {
			c.Parent = l.Parent // SPDK merges a single-clone snapshot into its clone
		}
		delete(lvs.Lvols, l.UUID)
		delete(f.qos, l.UUID)
		return true, nil
	},
	"bdev_lvol_snapshot": func(f *Fake, p map[string]any) (any, *rpcErr) {
		lvs, l := f.findLvolLocked(str(p, "lvol_name"))
		if l == nil {
			return nil, errf(codeENODEV, "No such device")
		}
		name := str(p, "snapshot_name")
		if len(name) > spdk.MaxLvolNameLen {
			return nil, errf(codeInvalidParams, "name too long")
		}
		for _, o := range lvs.Lvols {
			if o.Name == name {
				return nil, errf(codeEEXIST, "lvol %s already exists", name)
			}
		}
		// The snapshot takes ownership of every cluster written so far; the volume becomes an
		// empty thin clone of it.
		s := &mediaLvol{UUID: f.newUUID(), Name: name, SizeMiB: l.SizeMiB, IsSnapshot: true, Parent: l.Parent, Allocated: l.Allocated, Clusters: l.Clusters}
		l.Clusters, l.Allocated = nil, 0
		l.Parent = s.UUID
		lvs.Lvols[s.UUID] = s
		return s.UUID, nil
	},
	"bdev_lvol_clone": func(f *Fake, p map[string]any) (any, *rpcErr) {
		lvs, s := f.findLvolLocked(str(p, "snapshot_name"))
		if s == nil || !s.IsSnapshot {
			return nil, errf(codeENODEV, "No such device")
		}
		name := str(p, "clone_name")
		for _, o := range lvs.Lvols {
			if o.Name == name {
				return nil, errf(codeEEXIST, "lvol %s already exists", name)
			}
		}
		c := &mediaLvol{UUID: f.newUUID(), Name: name, SizeMiB: s.SizeMiB, Thin: true, Parent: s.UUID}
		lvs.Lvols[c.UUID] = c
		return c.UUID, nil
	},
	"bdev_lvol_get_lvols": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		var out []spdk.LvolInfo
		for key, lvsName := range f.loadedLvsByKey() {
			lvs := f.lvsMedia[key]
			for _, l := range lvs.Lvols {
				li := spdk.LvolInfo{Alias: lvsName + "/" + l.Name, UUID: l.UUID, Name: l.Name, IsSnapshot: l.IsSnapshot, IsClone: l.Parent != "", NumAllocatedClusters: l.Allocated, IsThinProvisioned: l.Thin}
				li.LVS.Name, li.LVS.UUID = lvsName, lvs.UUID
				out = append(out, li)
			}
		}
		return out, nil
	},
	"bdev_lvol_get_fragmap": func(f *Fake, p map[string]any) (any, *rpcErr) {
		lvs, l := f.findLvolLocked(str(p, "name"))
		if l == nil {
			return nil, errf(codeENODEV, "No such device")
		}
		cs := uint64(lvs.ClusterSize)
		off, size := num(p, "offset"), num(p, "size")
		if size == 0 {
			size = l.SizeMiB << 20
		}
		n := size / cs
		mask := make([]byte, (n+7)/8)
		var count uint64
		for i := uint64(0); i < n; i++ {
			if l.Clusters[off/cs+i] {
				mask[i/8] |= 1 << (i % 8)
				count++
			}
		}
		return spdk.FragmapResponse{ClusterSize: uint32(cs), Offset: off, Size: size, NumClusters: count, Bitmask: base64.StdEncoding.EncodeToString(mask)}, nil
	},
	"bdev_set_qos_limit": func(f *Fake, p map[string]any) (any, *rpcErr) {
		name := str(p, "name")
		if !f.bdevExistsLocked(name) {
			return nil, errf(codeENODEV, "No such device")
		}
		f.qos[f.canonicalBdevLocked(name)] = spdk.BdevRateLimits{
			RwIosPerSec: num(p, "rw_ios_per_sec"), RwMbytesPerSec: num(p, "rw_mbytes_per_sec"),
			RMbytesPerSec: num(p, "r_mbytes_per_sec"), WMbytesPerSec: num(p, "w_mbytes_per_sec"),
		}
		return true, nil
	},
	"bdev_get_iostat": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		return spdk.BdevGetIostatResponse{TickRate: 1}, nil
	},
	"nvmf_get_transports": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		var out []spdk.NvmfTransport
		for t := range f.transports {
			out = append(out, spdk.NvmfTransport{Trtype: t})
		}
		return out, nil
	},
	"nvmf_create_transport": func(f *Fake, p map[string]any) (any, *rpcErr) {
		t := strings.ToUpper(str(p, "trtype"))
		if f.transports[t] {
			return nil, errf(codeInvalidParams, "Transport type '%s' already exists", t)
		}
		f.transports[t] = true
		return true, nil
	},
	"nvmf_get_subsystems": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		var out []spdk.NvmfSubsystem
		for _, s := range f.subsystems {
			out = append(out, s.NvmfSubsystem)
		}
		return out, nil
	},
	"nvmf_create_subsystem": func(f *Fake, p map[string]any) (any, *rpcErr) {
		nqn := str(p, "nqn")
		if _, ok := f.subsystems[nqn]; ok {
			return nil, errf(codeInvalidParams, "Unable to create subsystem %s", nqn)
		}
		f.subsystems[nqn] = &subsystem{spdk.NvmfSubsystem{Nqn: nqn, Subtype: "NVMe", AllowAnyHost: boolean(p, "allow_any_host"), SerialNumber: str(p, "serial_number")}}
		return true, nil
	},
	"nvmf_delete_subsystem": func(f *Fake, p map[string]any) (any, *rpcErr) {
		nqn := str(p, "nqn")
		if _, ok := f.subsystems[nqn]; !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem with NQN %s", nqn)
		}
		delete(f.subsystems, nqn)
		return true, nil
	},
	"nvmf_subsystem_add_ns": func(f *Fake, p map[string]any) (any, *rpcErr) {
		s, ok := f.subsystems[str(p, "nqn")]
		if !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem")
		}
		ns, _ := p["namespace"].(map[string]any)
		bdev := str(ns, "bdev_name")
		if !f.bdevExistsLocked(bdev) {
			return nil, errf(codeInvalidParams, "Unable to add ns, bdev %s not found", bdev)
		}
		nsid := uint32(num(ns, "nsid"))
		if nsid == 0 {
			nsid = uint32(len(s.Namespaces) + 1)
		}
		for _, n := range s.Namespaces {
			if n.Nsid == nsid {
				return nil, errf(codeInvalidParams, "nsid %d in use", nsid)
			}
		}
		s.Namespaces = append(s.Namespaces, spdk.NvmfNamespace{Nsid: nsid, BdevName: f.canonicalBdevLocked(bdev), UUID: str(ns, "uuid")})
		return nsid, nil
	},
	"nvmf_subsystem_add_listener": func(f *Fake, p map[string]any) (any, *rpcErr) {
		s, ok := f.subsystems[str(p, "nqn")]
		if !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem")
		}
		var la spdk.NvmfListenAddress
		remarshal(p["listen_address"], &la)
		for _, l := range s.ListenAddresses {
			if l == la {
				return nil, errf(codeInvalidParams, "Listener already exists")
			}
		}
		s.ListenAddresses = append(s.ListenAddresses, la)
		return true, nil
	},
	"nvmf_subsystem_add_host": func(f *Fake, p map[string]any) (any, *rpcErr) {
		s, ok := f.subsystems[str(p, "nqn")]
		if !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem")
		}
		h := str(p, "host")
		for _, x := range s.Hosts {
			if x.Nqn == h {
				return nil, errf(codeInvalidParams, "host already added")
			}
		}
		s.Hosts = append(s.Hosts, spdk.NvmfHost{Nqn: h})
		return true, nil
	},
	"nvmf_subsystem_remove_host": func(f *Fake, p map[string]any) (any, *rpcErr) {
		s, ok := f.subsystems[str(p, "nqn")]
		if !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem")
		}
		h := str(p, "host")
		for i, x := range s.Hosts {
			if x.Nqn == h {
				s.Hosts = append(s.Hosts[:i], s.Hosts[i+1:]...)
				return true, nil
			}
		}
		return nil, errf(codeInvalidParams, "host not found")
	},
	"iscsi_get_portal_groups": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		var out []spdk.IscsiPortalGroup
		for tag, ps := range f.portalGroups {
			out = append(out, spdk.IscsiPortalGroup{Tag: tag, Portals: ps})
		}
		return out, nil
	},
	"iscsi_create_portal_group": func(f *Fake, p map[string]any) (any, *rpcErr) {
		tag := int(num(p, "tag"))
		if _, ok := f.portalGroups[tag]; ok {
			return nil, errf(codeInvalidParams, "portal group %d already exists", tag)
		}
		var ps []spdk.IscsiPortal
		remarshal(p["portals"], &ps)
		for _, existing := range f.portalGroups {
			for _, a := range existing {
				for _, b := range ps {
					if a == b {
						return nil, errf(codeInvalidParams, "portal (%s, %s) already exists", b.Host, b.Port)
					}
				}
			}
		}
		f.portalGroups[tag] = ps
		return true, nil
	},
	"iscsi_get_initiator_groups": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		var out []spdk.IscsiInitiatorGroup
		for _, ig := range f.initGroups {
			out = append(out, ig)
		}
		return out, nil
	},
	"iscsi_create_initiator_group": func(f *Fake, p map[string]any) (any, *rpcErr) {
		tag := int(num(p, "tag"))
		if _, ok := f.initGroups[tag]; ok {
			return nil, errf(codeInvalidParams, "initiator group %d already exists", tag)
		}
		f.initGroups[tag] = spdk.IscsiInitiatorGroup{Tag: tag, Initiators: strs(p, "initiators"), Netmasks: strs(p, "netmasks")}
		return true, nil
	},
	"iscsi_delete_initiator_group": func(f *Fake, p map[string]any) (any, *rpcErr) {
		tag := int(num(p, "tag"))
		if _, ok := f.initGroups[tag]; !ok {
			return nil, errf(codeInvalidParams, "initiator group %d not found", tag)
		}
		delete(f.initGroups, tag)
		return true, nil
	},
	"iscsi_initiator_group_add_initiators": func(f *Fake, p map[string]any) (any, *rpcErr) {
		tag := int(num(p, "tag"))
		ig, ok := f.initGroups[tag]
		if !ok {
			return nil, errf(codeInvalidParams, "initiator group %d not found", tag)
		}
		ig.Initiators = append(ig.Initiators, strs(p, "initiators")...)
		ig.Netmasks = append(ig.Netmasks, strs(p, "netmasks")...)
		f.initGroups[tag] = ig
		return true, nil
	},
	"iscsi_initiator_group_remove_initiators": func(f *Fake, p map[string]any) (any, *rpcErr) {
		tag := int(num(p, "tag"))
		ig, ok := f.initGroups[tag]
		if !ok {
			return nil, errf(codeInvalidParams, "initiator group %d not found", tag)
		}
		drop := func(list, rm []string) []string {
			var out []string
		next:
			for _, v := range list {
				for _, r := range rm {
					if v == r {
						continue next
					}
				}
				out = append(out, v)
			}
			return out
		}
		ig.Initiators = drop(ig.Initiators, strs(p, "initiators"))
		ig.Netmasks = drop(ig.Netmasks, strs(p, "netmasks"))
		f.initGroups[tag] = ig
		return true, nil
	},
	"nvmf_subsystem_allow_any_host": func(f *Fake, p map[string]any) (any, *rpcErr) {
		s, ok := f.subsystems[str(p, "nqn")]
		if !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem")
		}
		s.AllowAnyHost = boolean(p, "allow_any_host")
		return true, nil
	},
	"nvmf_subsystem_pause": func(f *Fake, p map[string]any) (any, *rpcErr) {
		if _, ok := f.subsystems[str(p, "nqn")]; !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem")
		}
		return true, nil
	},
	"nvmf_subsystem_resume": func(f *Fake, p map[string]any) (any, *rpcErr) {
		if _, ok := f.subsystems[str(p, "nqn")]; !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem")
		}
		return true, nil
	},
	"nvmf_subsystem_get_controllers": func(f *Fake, p map[string]any) (any, *rpcErr) {
		if _, ok := f.subsystems[str(p, "nqn")]; !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem")
		}
		return append([]spdk.NvmfController{}, f.nvmeConns[str(p, "nqn")]...), nil
	},
	"iscsi_get_connections": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		return []spdk.IscsiConnection{}, nil
	},
	"nvmf_subsystem_remove_ns": func(f *Fake, p map[string]any) (any, *rpcErr) {
		s, ok := f.subsystems[str(p, "nqn")]
		if !ok {
			return nil, errf(codeInvalidParams, "Unable to find subsystem")
		}
		nsid := uint32(num(p, "nsid"))
		for i, n := range s.Namespaces {
			if n.Nsid == nsid {
				s.Namespaces = append(s.Namespaces[:i], s.Namespaces[i+1:]...)
				return true, nil
			}
		}
		return nil, errf(codeInvalidParams, "nsid %d not found", nsid)
	},
	"iscsi_get_target_nodes": func(f *Fake, _ map[string]any) (any, *rpcErr) {
		var out []spdk.IscsiTargetNode
		for _, n := range f.targetNodes {
			out = append(out, *n)
		}
		return out, nil
	},
	"iscsi_create_target_node": func(f *Fake, p map[string]any) (any, *rpcErr) {
		name := str(p, "name")
		if _, ok := f.targetNodes[name]; ok {
			return nil, errf(codeInvalidParams, "target node %s already exists", name)
		}
		var n spdk.IscsiTargetNode
		remarshal(p, &n)
		for _, m := range n.PgIgMaps {
			if _, ok := f.portalGroups[m.PgTag]; !ok {
				return nil, errf(codeInvalidParams, "portal group %d not found", m.PgTag)
			}
			if _, ok := f.initGroups[m.IgTag]; !ok {
				return nil, errf(codeInvalidParams, "initiator group %d not found", m.IgTag)
			}
		}
		for _, l := range n.Luns {
			if !f.bdevExistsLocked(l.BdevName) {
				return nil, errf(codeInvalidParams, "bdev %s not found", l.BdevName)
			}
		}
		f.targetNodes[name] = &n
		return true, nil
	},
	"iscsi_delete_target_node": func(f *Fake, p map[string]any) (any, *rpcErr) {
		name := str(p, "name")
		if _, ok := f.targetNodes[name]; !ok {
			return nil, errf(codeInvalidParams, "target node %s not found", name)
		}
		delete(f.targetNodes, name)
		return true, nil
	},
	"iscsi_target_node_add_pg_ig_maps": func(f *Fake, p map[string]any) (any, *rpcErr) {
		n, ok := f.targetNodes[str(p, "name")]
		if !ok {
			return nil, errf(codeInvalidParams, "target node not found")
		}
		var maps []spdk.IscsiPgIgMap
		remarshal(p["pg_ig_maps"], &maps)
		for _, m := range maps {
			if _, ok := f.portalGroups[m.PgTag]; !ok {
				return nil, errf(codeInvalidParams, "portal group %d not found", m.PgTag)
			}
			if _, ok := f.initGroups[m.IgTag]; !ok {
				return nil, errf(codeInvalidParams, "initiator group %d not found", m.IgTag)
			}
			n.PgIgMaps = append(n.PgIgMaps, m)
		}
		return true, nil
	},
	"iscsi_target_node_remove_pg_ig_maps": func(f *Fake, p map[string]any) (any, *rpcErr) {
		n, ok := f.targetNodes[str(p, "name")]
		if !ok {
			return nil, errf(codeInvalidParams, "target node not found")
		}
		var maps []spdk.IscsiPgIgMap
		remarshal(p["pg_ig_maps"], &maps)
		var kept []spdk.IscsiPgIgMap
	outer:
		for _, m := range n.PgIgMaps {
			for _, r := range maps {
				if m == r {
					continue outer
				}
			}
			kept = append(kept, m)
		}
		n.PgIgMaps = kept
		return true, nil
	},
}
