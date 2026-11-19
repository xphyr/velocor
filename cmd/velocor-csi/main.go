package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/velocor/velocor/internal/csi"
	"github.com/velocor/velocor/internal/version"
)

func main() {
	// `velocor-csi storageprofile <class>...`: run by the Helm chart's hook Job, not a driver.
	if len(os.Args) > 1 && os.Args[1] == "storageprofile" {
		runStorageProfile(os.Args[2:])
		return
	}
	var (
		driverName = flag.String("driver-name", csi.DefaultDriverName, "CSI driver name")
		endpoint   = flag.String("endpoint", "unix:///csi/csi.sock", "CSI gRPC endpoint")
		nodeID     = flag.String("node-id", envOr("NODE_NAME", hostnameOr("velocor-node")), "Kubernetes node ID")
		mode       = flag.String("mode", string(csi.ModeAll), "Driver mode: all | controller | node")
		apiURL     = flag.String("velocor-endpoint", os.Getenv("VELOCOR_ENDPOINT"), "Velocor API base URL (e.g. https://10.0.2.10:8443)")
		token      = flag.String("velocor-token", os.Getenv("VELOCOR_TOKEN"), "Velocor API token (velo_pat_...)")
		insecure   = flag.Bool("velocor-insecure", os.Getenv("VELOCOR_INSECURE") == "true", "Skip TLS verification for Velocor API (lab use only)")
		caCert     = flag.String("velocor-ca-cert", os.Getenv("VELOCOR_CA_CERT"), "PEM CA/server certificate used to verify the Velocor API")
		portals    = flag.String("default-portals", os.Getenv("VELOCOR_DEFAULT_PORTALS"),
			"Velocor portals (names or IDs, comma-separated, either protocol) for StorageClasses without a \"portals\" parameter")
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

		DefaultPortals: *portals,
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

// runStorageProfile applies the CDI StorageProfile spec in $STORAGEPROFILE_SPEC (JSON) to the
// named StorageClasses' profiles.
func runStorageProfile(classes []string) {
	spec := os.Getenv("STORAGEPROFILE_SPEC")
	if len(classes) == 0 || spec == "" {
		log.Fatalf("usage: STORAGEPROFILE_SPEC='{...}' velocor-csi storageprofile <storageclass>...")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := csi.ApplyStorageProfiles(ctx, classes, json.RawMessage(spec)); err != nil {
		log.Fatalf("storageprofile: %v", err)
	}
}
