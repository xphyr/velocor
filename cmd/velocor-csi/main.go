package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/velocor/velocor/internal/csi"
	"github.com/velocor/velocor/internal/version"
)

func main() {
	var (
		driverName = flag.String("driver-name", csi.DefaultDriverName, "CSI driver name")
		endpoint   = flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
		nodeID     = flag.String("node-id", envOr("NODE_NAME", hostnameOr("velocor-node")), "Kubernetes node ID")
		mode       = flag.String("mode", string(csi.ModeAll), "Driver mode: all | controller | node")
		apiURL     = flag.String("velocor-endpoint", os.Getenv("VELOCOR_ENDPOINT"), "Velocor API base URL (e.g. https://10.0.2.10:8443)")
		token      = flag.String("velocor-token", os.Getenv("VELOCOR_TOKEN"), "Velocor API token (velo_pat_...)")
		insecure   = flag.Bool("velocor-insecure", os.Getenv("VELOCOR_INSECURE") == "true", "Skip TLS verification for Velocor API (lab use only)")
		caCert     = flag.String("velocor-ca-cert", os.Getenv("VELOCOR_CA_CERT"), "PEM CA/server certificate used to verify the Velocor API")
	)
	flag.Parse()

	var client csi.VelocorClient
	if *apiURL != "" {
		if *insecure {
			log.Printf("WARNING: TLS verification of %s is disabled; the API token can be intercepted", *apiURL)
		}
		c, err := csi.NewHTTPClientWithCA(*apiURL, *token, *insecure, *caCert)
		if err != nil {
			log.Fatalf("init velocor client: %v", err)
		}
		client = c
	}

	drv, err := csi.NewDriver(csi.DriverConfig{
		DriverName: *driverName,
		Endpoint:   *endpoint,
		NodeID:     *nodeID,
		Mode:       csi.DriverMode(*mode),
		Client:     client,
	})
	if err != nil {
		log.Fatalf("init velocor-csi: %v", err)
	}

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		log.Printf("shutting down velocor-csi")
		drv.Stop()
	}()

	log.Printf("starting velocor-csi %s (mode=%s, endpoint=%s, node=%s)", version.Version, *mode, *endpoint, *nodeID)
	if err := drv.Run(); err != nil {
		log.Fatalf("velocor-csi failed: %v", err)
	}
}

func envOr(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func hostnameOr(fallback string) string {
	if h, err := os.Hostname(); err == nil && h != "" {
		return h
	}
	return fallback
}
