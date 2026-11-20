//go:build integration

// Package integration runs Velocor against a real spdk_tgt.
//
// Every test gets its own spdk_tgt container (rootless podman, --no-huge, file-backed disks
// through bdev_aio) and a real velod (store, API, reconciler) pointed at it. Data goes in and
// out through SPDK's own NVMe/TCP initiator (spdk_dd in a second container), so exports, ACLs,
// snapshots and clones are exercised end to end without root or kernel NVMe modules.
//
//	podman build --target test -t localhost/velocor-spdk:test -f deploy/containers/Containerfile.spdk .
//	go test -tags integration ./internal/integration/ -v
//
// VELOCOR_IT_IMAGE overrides the image.
package integration

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/server"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

func image() string {
	if v := os.Getenv("VELOCOR_IT_IMAGE"); v != "" {
		return v
	}
	return "localhost/velocor-spdk:test"
}

// target is one spdk_tgt container and its working directory.
type target struct {
	t    *testing.T
	dir  string // holds the RPC socket and the disk image files, at the same path inside and out
	name string
}

// shortDir returns a directory whose path is short enough for a UNIX socket.
func shortDir(t *testing.T) string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	d, err := os.MkdirTemp(base, "vit-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return d
}

func podman(t *testing.T, args ...string) (string, error) {
	t.Helper()
	out, err := exec.Command("podman", args...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func newTarget(t *testing.T) *target {
	t.Helper()
	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman is required")
	}
	if out, err := podman(t, "image", "exists", image()); err != nil {
		t.Skipf("image %s not found (%s); build it with podman build --target test", image(), out)
	}
	tg := &target{t: t, dir: shortDir(t), name: fmt.Sprintf("vit-%d", time.Now().UnixNano())}
	t.Cleanup(func() {
		if t.Failed() {
			logs, _ := podman(t, "logs", "--tail", "40", tg.name)
			t.Logf("spdk_tgt log tail:\n%s", logs)
		}
		_, _ = podman(t, "rm", "-f", "-t", "0", tg.name)
	})
	tg.start()
	return tg
}

func (tg *target) socket() string { return filepath.Join(tg.dir, "spdk.sock") }

func (tg *target) client() *spdk.Client { return spdk.NewClient(tg.socket()) }

// start runs spdk_tgt with the production config, and waits for its RPC socket.
func (tg *target) start() {
	tg.t.Helper()
	_, _ = podman(tg.t, "rm", "-f", "-t", "0", tg.name)
	_ = os.Remove(tg.socket())
	// Unconfined like the appliance's quadlet (SecurityLabelDisable=true), so SELinux hosts run
	// the same SSD backend (io_uring) as the appliance and CI runners (docs/review-history.md P0-6).
	out, err := podman(tg.t, "run", "-d", "--name", tg.name, "--network", "host", "--security-opt", "seccomp=unconfined",
		"--security-opt", "label=disable",
		"-v", tg.dir+":"+tg.dir+":z", "--entrypoint", "spdk_tgt", image(),
		"-r", tg.socket(), "-m", "0x1", "-s", "1024", "--no-huge", "--no-pci", "--interrupt-mode", "--json", "/etc/velocor/spdk.json")
	if err != nil {
		tg.t.Fatalf("start spdk_tgt: %v: %s", err, out)
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := tg.client().GetVersion(context.Background()); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	logs, _ := podman(tg.t, "logs", tg.name)
	tg.t.Fatalf("spdk_tgt did not come up:\n%s", logs)
}

// crash kills spdk_tgt without warning, like a power cut to the data plane.
func (tg *target) crash() {
	tg.t.Helper()
	if out, err := podman(tg.t, "kill", "-s", "KILL", tg.name); err != nil {
		tg.t.Fatalf("kill spdk_tgt: %v: %s", err, out)
	}
	_, _ = podman(tg.t, "wait", tg.name)
}

// stop shuts spdk_tgt down cleanly (SIGTERM), as systemd would.
func (tg *target) stop() {
	tg.t.Helper()
	if out, err := podman(tg.t, "stop", "-t", "30", tg.name); err != nil {
		tg.t.Fatalf("stop spdk_tgt: %v: %s", err, out)
	}
}

// disk makes a file-backed disk of the given size and returns its path.
func (tg *target) disk(name string, size int64) string {
	tg.t.Helper()
	p := filepath.Join(tg.dir, name)
	f, err := os.Create(p)
	if err != nil {
		tg.t.Fatal(err)
	}
	if err := f.Truncate(size); err != nil {
		tg.t.Fatal(err)
	}
	_ = f.Close()
	return p
}

// freePort returns a TCP port nothing is listening on, for a test's NVMe/TCP portal.
func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port
}

// appliance is a velod (API + reconciler) over a target.
type appliance struct {
	t     *testing.T
	tg    *target
	st    *store.BoltStore
	rec   *reconciler.Reconciler
	ts    *httptest.Server
	token string
	port  int
}

func newAppliance(t *testing.T, tg *target) *appliance {
	t.Helper()
	// The test containers run confined (SELinux container_t), where io_uring_setup fails and
	// spdk_tgt would crash on the first io_uring bdev (docs/review-history.md P0-6). "auto" must see that and
	// use AIO; if it didn't, every test here would lose its spdk_tgt.
	dataplane.SSDBackend = "auto"
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "velocor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	authMgr, err := auth.NewManager(st, "integration-pass")
	if err != nil {
		t.Fatal(err)
	}
	_, tok, err := authMgr.CreateApiToken("it", store.RoleAdmin, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rec := reconciler.New(st, tg.client(), nil, nil)
	rec.SetUnmanagedGrace(0) // report unaccounted SPDK objects at once, so tests can assert there are none
	ts := httptest.NewServer(server.New(st, tg.client(), nil, authMgr, rec, nil).Handler())
	t.Cleanup(ts.Close)
	return &appliance{t: t, tg: tg, st: st, rec: rec, ts: ts, token: tok, port: freePort(t)}
}

func (a *appliance) do(method, path string, body any) (int, map[string]any) {
	a.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, a.ts.URL+"/api/v1"+path, rdr)
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatalf("%s %s: %v", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// must performs a request that has to succeed and returns the object under key.
func (a *appliance) must(method, path string, body any, key string) map[string]any {
	a.t.Helper()
	code, out := a.do(method, path, body)
	if code >= 300 {
		a.t.Fatalf("%s %s: HTTP %d: %v", method, path, code, out["error"])
	}
	if key == "" {
		return out
	}
	m, _ := out[key].(map[string]any)
	return m
}

// addDisk registers a file-backed SSD with the store, as a hardware scan would, and claims it.
func (a *appliance) addDisk(id string, size int64) {
	a.t.Helper()
	path := a.tg.disk(id+".img", size)
	if err := a.st.UpsertDisk(&store.Disk{ID: id, Name: id, Type: store.DiskTypeSSD, DevicePath: path, SizeBytes: uint64(size), BlockSize: 512, SerialNumber: id}); err != nil {
		a.t.Fatal(err)
	}
	a.must("POST", "/disks/"+id+":claim", nil, "disk")
}

func (a *appliance) reconcile() reconciler.State {
	a.t.Helper()
	st, _ := a.rec.ReconcileOnce(context.Background())
	return st
}

// exportTo creates an NVMe/TCP portal (once) and exports volume to hostNQN. It returns the subsystem NQN.
func (a *appliance) exportTo(volID, name, hostNQN string) string {
	a.t.Helper()
	code, portals := a.do("GET", "/portals", nil)
	var portalID string
	if code == 200 {
		list, _ := portals["portals"].([]any)
		for _, p := range list {
			portalID = p.(map[string]any)["id"].(string)
		}
	}
	if portalID == "" {
		portalID = a.must("POST", "/portals", map[string]any{"name": "lo", "protocol": "NVMEOF_TCP", "ip_address": "127.0.0.1", "port": a.port}, "portal")["id"].(string)
	}
	ig := a.must("POST", "/initiator-groups", map[string]any{"name": "ig-" + name, "host_nqns": []string{hostNQN}}, "initiator_group")
	exp := a.must("POST", "/exports", map[string]any{"name": name, "volume_id": volID, "protocol": "NVMEOF_TCP",
		"portal_ids": []string{portalID}, "initiator_group_ids": []string{ig["id"].(string)}}, "export")
	return exp["subsystem_nqn"].(string)
}

const blockSize = 4096

// initiator reads and writes an exported namespace over NVMe/TCP with spdk_dd.
type initiator struct {
	t       *testing.T
	tg      *target
	port    int
	nqn     string
	hostNQN string
	qd      int // spdk_dd queue depth; 0 means 32
}

func (a *appliance) initiator(nqn, hostNQN string) *initiator {
	return &initiator{t: a.t, tg: a.tg, port: a.port, nqn: nqn, hostNQN: hostNQN}
}

func (in *initiator) dd(args ...string) error {
	in.t.Helper()
	cfg := map[string]any{"subsystems": []any{map[string]any{"subsystem": "bdev", "config": []any{
		// Don't retry failed I/O (a full pool fails writes; retries only make the test wait).
		map[string]any{"method": "bdev_nvme_set_options", "params": map[string]any{"bdev_retry_count": 0}},
		map[string]any{"method": "bdev_nvme_attach_controller", "params": map[string]any{
			"name": "r", "trtype": "tcp", "adrfam": "ipv4", "traddr": "127.0.0.1", "trsvcid": strconv.Itoa(in.port),
			"subnqn": in.nqn, "hostnqn": in.hostNQN,
		}},
	}}}}
	b, _ := json.Marshal(cfg)
	cfgPath := filepath.Join(in.tg.dir, fmt.Sprintf("dd-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(cfgPath, b, 0o644); err != nil {
		in.t.Fatal(err)
	}
	full := append([]string{"run", "--rm", "--timeout", "120", "--network", "host", "--security-opt", "seccomp=unconfined",
		"-v", in.tg.dir + ":" + in.tg.dir + ":z", "--entrypoint", "spdk_dd", image(),
		"--no-huge", "--no-pci", "-s", "1024", "--aio", "--qd", strconv.Itoa(cmp.Or(in.qd, 32)), "--json", cfgPath}, args...)
	out, err := exec.Command("podman", full...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("spdk_dd %v: %v\n%s", args, err, lastLines(string(out), 15))
	}
	return nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// write puts data at block offset (in 4 KiB blocks) of the namespace.
func (in *initiator) write(blockOffset int, data []byte) error {
	in.t.Helper()
	src := filepath.Join(in.tg.dir, fmt.Sprintf("w-%d.bin", time.Now().UnixNano()))
	if err := os.WriteFile(src, data, 0o644); err != nil {
		in.t.Fatal(err)
	}
	if err := in.dd("--if", src, "--ob", "rn1", "--bs", strconv.Itoa(blockSize),
		"--count", strconv.Itoa(len(data)/blockSize), "--seek", strconv.Itoa(blockOffset)); err != nil {
		return err
	}
	// spdk_dd exits 0 even when writes fail (found measuring a full pool, P0-2): read back to
	// know the write landed.
	got, err := in.read(blockOffset, len(data))
	if err != nil {
		return fmt.Errorf("read back after write: %w", err)
	}
	if !bytes.Equal(got, data) {
		return fmt.Errorf("write at block %d did not land: read back differs from byte %d (spdk_dd reported success)", blockOffset, firstDiff(got, data))
	}
	return nil
}

// read returns n bytes at block offset of the namespace.
func (in *initiator) read(blockOffset, n int) ([]byte, error) {
	in.t.Helper()
	dst := filepath.Join(in.tg.dir, fmt.Sprintf("r-%d.bin", time.Now().UnixNano()))
	if err := in.dd("--ib", "rn1", "--of", dst, "--bs", strconv.Itoa(blockSize),
		"--count", strconv.Itoa(n/blockSize), "--skip", strconv.Itoa(blockOffset)); err != nil {
		return nil, err
	}
	return os.ReadFile(dst)
}

func randomData(t *testing.T, n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	return b
}

func (in *initiator) mustRead(blockOffset int, want []byte) {
	in.t.Helper()
	got, err := in.read(blockOffset, len(want))
	if err != nil {
		in.t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		in.t.Fatalf("data at block %d differs: %d bytes read, first mismatch at %d", blockOffset, len(got), firstDiff(got, want))
	}
}

func firstDiff(a, b []byte) int {
	for i := range min(len(a), len(b)) {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// rawBdev returns bdev_get_bdevs output for name as generic JSON, for fields the client doesn't model.
func (tg *target) rawBdev(name string) map[string]any {
	tg.t.Helper()
	var res []map[string]any
	if err := tg.client().Call(context.Background(), "bdev_get_bdevs", map[string]any{"name": name}, &res); err != nil || len(res) == 0 {
		tg.t.Fatalf("bdev_get_bdevs %s: %v", name, err)
	}
	return res[0]
}
