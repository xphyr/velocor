package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/backup"
	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/dataplane/kernelplane"
	"github.com/velocor/velocor/internal/dataplane/spdkplane"
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/kernel"
	"github.com/velocor/velocor/internal/meta"
	"github.com/velocor/velocor/internal/monitoring"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/server"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
	"github.com/velocor/velocor/internal/version"
)

func main() {
	// "velod healthcheck [-listen :8443] [-insecure-http]": the quadlet's HealthCmd (docs/review-history.md
	// P2-6). Asks this velod's /healthz on the loopback interface; exit 0 when it answers.
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}
	// "velod meta-io ...": the helper that does the on-pool metadata I/O (internal/meta).
	if len(os.Args) > 1 && os.Args[1] == "meta-io" {
		if err := meta.RunHelper(os.Args[2:], os.Stdin, os.Stdout, true); err != nil {
			fmt.Fprintln(os.Stderr, "meta-io:", err)
			os.Exit(1)
		}
		return
	}
	var (
		listenAddr        = flag.String("listen", envOrDefault("VELOCOR_LISTEN", ":8443"), "Address and port for the unified API/UI server")
		dbPath            = flag.String("db", envOrDefault("VELOCOR_DB_PATH", "/var/lib/velocor/velocor.db"), "Path to persistent bbolt state database")
		dataPlane         = flag.String("data-plane", envOrDefault("VELOCOR_DATA_PLANE", "spdk"), "The data plane: spdk (spdk_tgt) or kernel (md, LVM thin, nvmet and LIO; docs/design/kernel-dataplane.md)")
		spdkSocket        = flag.String("spdk-socket", envOrDefault("VELOCOR_SPDK_SOCKET", spdk.DefaultSocketPath), "Path to spdk_tgt JSON-RPC 2.0 UNIX socket")
		hugepagesMB       = flag.Uint64("hugepages-mb", 0, "Ensure at least N MiB of 2 MiB hugepages on startup (0 to skip)")
		reconcileInterval = flag.Duration("reconcile-interval", 15*time.Second, "Background state reconciliation interval")
		tlsCertFile       = flag.String("tls-cert", os.Getenv("VELOCOR_TLS_CERT"), "TLS certificate file (auto-generates self-signed ECDSA cert if empty)")
		tlsKeyFile        = flag.String("tls-key", os.Getenv("VELOCOR_TLS_KEY"), "TLS private key file")
		insecureHTTP      = flag.Bool("insecure-http", os.Getenv("VELOCOR_INSECURE_HTTP") == "true", "Serve plain HTTP instead of HTTPS")
		ssdBdev           = flag.String("ssd-bdev", envOrDefault("VELOCOR_SSD_BDEV", "auto"), "How SSDs are attached to SPDK: auto (default: io_uring if spdk_tgt can use it, else AIO), uring, or aio")
		metricsPublic     = flag.Bool("metrics-public", os.Getenv("VELOCOR_METRICS_PUBLIC") == "true", "Serve /metrics without authentication (default: requires a READONLY token)")
		metaCopies        = flag.Bool("meta-copies", os.Getenv("VELOCOR_META_COPIES") != "false", "Keep a copy of each pool's configuration on the pool (needs SPDK built with ublk and the ublk_drv module)")
		monitoringDir     = flag.String("monitoring-dir", envOrDefault("VELOCOR_MONITORING_DIR", "/var/lib/velocor-monitoring"), "Directory for the built-in Prometheus/Alertmanager's config and data; the feature is offered only where it exists (the appliance image creates it)")
		showVersion       = flag.Bool("version", false, "Print version information and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("velod %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildTime)
		return
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("starting velod control plane",
		"version", version.Version,
		"commit", version.Commit,
		"listen", *listenAddr,
		"db", *dbPath,
		"spdk_socket", *spdkSocket,
	)

	if *dataPlane != "spdk" && *dataPlane != "kernel" {
		logger.Error("invalid --data-plane; use spdk or kernel", "value", *dataPlane)
		os.Exit(1)
	}
	spdkplane.Logger = logger
	switch *ssdBdev {
	case "auto", "uring", "aio":
		spdkplane.SSDBackend = *ssdBdev
	default:
		logger.Error("invalid --ssd-bdev; use auto, uring or aio", "value", *ssdBdev)
		os.Exit(1)
	}
	if *ssdBdev == "uring" && *dataPlane == "spdk" {
		if probe := hardware.ProbeIOURing(); !probe.Usable {
			logger.Warn("--ssd-bdev=uring although io_uring may not work for spdk_tgt; a failed io_uring setup crashes it (use auto or aio)", "reason", probe.Reason)
		}
	}

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
		logger.Error("failed to create state directory", "path", filepath.Dir(*dbPath), "err", err)
		os.Exit(1)
	}

	if err := backup.ApplyPendingRestore(*dbPath, logger); err != nil {
		logger.Error("database restore failed; starting on the current database", "err", err)
	}
	st, err := store.OpenBoltStore(*dbPath)
	if err != nil {
		logger.Error("failed to open bbolt state store", "path", *dbPath, "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	hwMgr, err := hardware.NewManager(hardware.Config{
		AllowUnsafeNoIOMMU: os.Getenv("VELOCOR_ALLOW_UNSAFE_NOIOMMU") == "true",
		KernelDataPlane:    *dataPlane == "kernel",
	})
	if err != nil {
		// Without the hardware manager there is no boot-disk guard; fail closed.
		logger.Error("hardware manager initialization failed; refusing to start without the boot-disk guard", "err", err)
		os.Exit(1)
	}

	if *dataPlane == "kernel" && hwMgr != nil {
		// The image reserves hugepages at boot for spdk_tgt, which doesn't run with this data plane.
		if hp, err := hwMgr.ReleaseHugepages(); err != nil {
			logger.Warn("failed to release hugepages", "err", err)
		} else {
			logger.Info("hugepages released (not needed by the kernel data plane)", "total_mb", hp.TotalMB)
		}
	}
	if *hugepagesMB > 0 && hwMgr != nil && *dataPlane == "spdk" {
		if hp, err := hwMgr.EnsureHugepages(*hugepagesMB); err != nil {
			logger.Warn("failed to allocate hugepages", "requested_mb", *hugepagesMB, "err", err)
		} else {
			logger.Info("hugepages ready", "total_mb", hp.TotalMB, "free_mb", hp.FreeMB)
		}
	}

	initPassword := os.Getenv("VELOCOR_INIT_PASSWORD")
	if _, err := st.GetAdminUser(auth.DefaultAdminUsername); errors.Is(err, store.ErrNotFound) && initPassword == "" {
		// First boot without an operator-supplied password: generate a unique one instead of
		// shipping a well-known default, and hand it over via a root-only file.
		initPassword, err = auth.GenerateBootstrapPassword()
		if err != nil {
			logger.Error("failed to generate bootstrap admin password", "err", err)
			os.Exit(1)
		}
		pwFile := filepath.Join(filepath.Dir(*dbPath), "initial-admin-password")
		if err := os.WriteFile(pwFile, []byte(initPassword+"\n"), 0o600); err != nil {
			logger.Error("failed to write bootstrap admin password", "path", pwFile, "err", err)
			os.Exit(1)
		}
		logger.Warn("generated initial admin password; it must be changed at first login", "user", auth.DefaultAdminUsername, "path", pwFile)
	}
	authMgr, err := auth.NewManager(st, initPassword)
	if err != nil {
		logger.Error("failed to initialize auth manager", "err", err)
		os.Exit(1)
	}

	var (
		dp         dataplane.Plane
		spdkClient *spdk.Client
		kp         *kernelplane.Plane
	)
	if *dataPlane == "kernel" {
		// In velod's container (host PID namespace), the storage tools run in the host's mount
		// namespace, where their configuration and udev's /dev links are.
		kp = kernelplane.New(kernel.New(kernel.Exec{HostNamespace: inContainer()}), hwMgr)
		dp = kp
	} else {
		spdkClient = spdk.NewClient(*spdkSocket)
		dp = spdkplane.New(spdkClient, hwMgr)
	}
	logger.Info("data plane", "kind", *dataPlane, "endpoint", dp.Endpoint())
	rec := reconciler.New(st, dp, hwMgr, logger)
	if kp != nil {
		// No spdk_tgt whose restart (empty again) velod must restore after.
		rec.SetSPDKInstance(func() string { return "" })
	}
	var metaDev meta.Device
	if *metaCopies {
		self, err := os.Executable()
		if err != nil {
			logger.Error("cannot locate the velod binary for metadata I/O", "err", err)
			os.Exit(1)
		}
		nodeID := ""
		if cfg, err := st.GetSystemConfig(); err == nil {
			nodeID = cfg.NodeID
		}
		// One device for the reconciler (writes) and pool import (reads): it serializes them.
		if kp != nil {
			metaDev = &meta.BlockDevice{Path: kp.VolumePath, Helper: self}
		} else {
			metaDev = &meta.UblkDevice{SPDK: spdkClient, Helper: self, StateDir: filepath.Dir(*spdkSocket), ID: metaUblkID, Logger: logger}
		}
		rec.SetMetaDevice(metaDev, nodeID, version.Version)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go rec.Start(ctx, *reconcileInterval)

	backups := &backup.Manager{Dir: filepath.Join(filepath.Dir(*dbPath), "backups"), DBPath: *dbPath, Source: st, Logger: logger}
	go backups.Run(ctx)

	var mon *monitoring.Manager
	if fi, err := os.Stat(*monitoringDir); err == nil && fi.IsDir() {
		scheme := "https"
		if *insecureHTTP {
			scheme = "http"
		}
		mon = &monitoring.Manager{Dir: *monitoringDir, MetricsURL: scheme + "://" + monitoring.LocalTarget(*listenAddr) + "/metrics"}
	}
	srv := server.New(st, dp, hwMgr, authMgr, rec, logger,
		server.WithMonitoring(mon),
		server.WithPublicMetrics(*metricsPublic),
		server.WithInsecureHTTP(*insecureHTTP),
		server.WithBackups(backups, stop),
		server.WithMetaDevice(metaDev), // velod exits; systemd starts it on the restored database
		server.WithBootstrapPasswordFile(filepath.Join(filepath.Dir(*dbPath), "initial-admin-password")))
	httpServer := &http.Server{
		Addr:              *listenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute, // reconcile / group snapshot requests can be slow
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    64 << 10,
	}

	// The built-in Prometheus/Alertmanager have no [Install]: velod starts them when they're on.
	go srv.StartMonitoring(ctx)
	// Did the OS image staged before the last reboot take effect? (docs/review-history.md P1-7)
	go srv.CheckOSUpgrade(ctx)
	go srv.RunScrubScheduler(ctx)

	errCh := make(chan error, 1)
	go func() {
		if *insecureHTTP {
			logger.Info("serving HTTP", "addr", *listenAddr)
			errCh <- httpServer.ListenAndServe()
			return
		}
		if *tlsCertFile != "" && *tlsKeyFile != "" {
			logger.Info("serving HTTPS with configured TLS certificate", "addr", *listenAddr, "cert", *tlsCertFile)
			errCh <- httpServer.ListenAndServeTLS(*tlsCertFile, *tlsKeyFile)
			return
		}
		certPath := filepath.Join(filepath.Dir(*dbPath), "tls.crt")
		keyPath := filepath.Join(filepath.Dir(*dbPath), "tls.key")
		tlsCfg, err := server.LoadOrGenerateSelfSignedTLSConfig(certPath, keyPath)
		if err != nil {
			errCh <- fmt.Errorf("load/generate self-signed tls cert: %w", err)
			return
		}
		httpServer.TLSConfig = tlsCfg
		logger.Info("serving HTTPS with persistent self-signed TLS certificate", "addr", *listenAddr, "cert", certPath)
		ln, err := tls.Listen("tcp", *listenAddr, tlsCfg)
		if err != nil {
			errCh <- err
			return
		}
		errCh <- httpServer.Serve(ln)
	}()

	select {
	case <-ctx.Done():
		logger.Info("shutting down velod...")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = httpServer.Shutdown(shutCtx)
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("server exited with error", "err", err)
			os.Exit(1)
		}
	}
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// metaUblkID is the ublk device ID reserved for metadata I/O (/dev/ublkb<id>).
const metaUblkID = 100

// healthcheck GETs /healthz from the velod listening on -listen (on the loopback interface).
func healthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	listen := fs.String("listen", envOrDefault("VELOCOR_LISTEN", ":8443"), "velod's listen address")
	plain := fs.Bool("insecure-http", os.Getenv("VELOCOR_INSECURE_HTTP") == "true", "velod serves plain HTTP")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	scheme := "https"
	if *plain {
		scheme = "http"
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		// Our own process on the loopback interface, with whatever certificate it serves.
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}}
	resp, err := client.Get(scheme + "://" + monitoring.LocalTarget(*listen) + "/healthz")
	if err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		fmt.Fprintln(os.Stderr, "healthcheck:", resp.Status)
		return 1
	}
	return 0
}

// inContainer reports whether velod runs in a mount namespace other than PID 1's (its
// container, which shares the host's PID namespace).
func inContainer() bool {
	self, err1 := os.Readlink("/proc/self/ns/mnt")
	host, err2 := os.Readlink("/proc/1/ns/mnt")
	return err1 == nil && err2 == nil && self != host
}
