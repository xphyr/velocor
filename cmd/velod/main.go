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
	"github.com/velocor/velocor/internal/hardware"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/server"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
	"github.com/velocor/velocor/internal/version"
)

func main() {
	var (
		listenAddr        = flag.String("listen", envOrDefault("VELOCOR_LISTEN", ":8443"), "Address and port for the unified API/UI server")
		dbPath            = flag.String("db", envOrDefault("VELOCOR_DB_PATH", "/var/lib/velocor/velocor.db"), "Path to persistent bbolt state database")
		spdkSocket        = flag.String("spdk-socket", envOrDefault("VELOCOR_SPDK_SOCKET", spdk.DefaultSocketPath), "Path to spdk_tgt JSON-RPC 2.0 UNIX socket")
		hugepagesMB       = flag.Uint64("hugepages-mb", 0, "Ensure at least N MiB of 2 MiB hugepages on startup (0 to skip)")
		reconcileInterval = flag.Duration("reconcile-interval", 15*time.Second, "Background state reconciliation interval")
		tlsCertFile       = flag.String("tls-cert", os.Getenv("VELOCOR_TLS_CERT"), "TLS certificate file (auto-generates self-signed ECDSA cert if empty)")
		tlsKeyFile        = flag.String("tls-key", os.Getenv("VELOCOR_TLS_KEY"), "TLS private key file")
		insecureHTTP      = flag.Bool("insecure-http", os.Getenv("VELOCOR_INSECURE_HTTP") == "true", "Serve plain HTTP instead of HTTPS")
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

	if err := os.MkdirAll(filepath.Dir(*dbPath), 0700); err != nil {
		logger.Error("failed to create state directory", "path", filepath.Dir(*dbPath), "err", err)
		os.Exit(1)
	}

	st, err := store.OpenBoltStore(*dbPath)
	if err != nil {
		logger.Error("failed to open bbolt state store", "path", *dbPath, "err", err)
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()

	hwMgr, err := hardware.NewManager(hardware.Config{})
	if err != nil {
		logger.Warn("hardware manager initialization warning", "err", err)
	}

	if *hugepagesMB > 0 && hwMgr != nil {
		if hp, err := hwMgr.EnsureHugepages(*hugepagesMB); err != nil {
			logger.Warn("failed to allocate hugepages", "requested_mb", *hugepagesMB, "err", err)
		} else {
			logger.Info("hugepages ready", "total_mb", hp.TotalMB, "free_mb", hp.FreeMB)
		}
	}

	authMgr, err := auth.NewManager(st, os.Getenv("VELOCOR_INIT_PASSWORD"))
	if err != nil {
		logger.Error("failed to initialize auth manager", "err", err)
		os.Exit(1)
	}

	spdkClient := spdk.NewClient(*spdkSocket)
	rec := reconciler.New(st, spdkClient, hwMgr, logger)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go rec.Start(ctx, *reconcileInterval)

	srv := server.New(st, spdkClient, hwMgr, authMgr, rec, logger)
	httpServer := &http.Server{
		Addr:              *listenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

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
