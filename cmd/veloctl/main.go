package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/velocor/velocor/internal/version"
)

type cliConfig struct {
	endpoint   string
	token      string
	insecure   bool
	jsonOutput bool
	httpClient *http.Client
}

func main() {
	var (
		endpoint    = flag.String("endpoint", envOrDefault("VELOCOR_ENDPOINT", "https://127.0.0.1:8443"), "Velocor API endpoint URL")
		token       = flag.String("token", os.Getenv("VELOCOR_TOKEN"), "Bearer token (JWT or velo_pat_...); reads ~/.config/velocor/token if unset")
		insecure    = flag.Bool("insecure", os.Getenv("VELOCOR_INSECURE") != "false", "Skip TLS certificate verification for self-signed appliance certs")
		jsonOut     = flag.Bool("json", false, "Print raw JSON responses")
		showVersion = flag.Bool("version", false, "Print veloctl version and exit")
	)
	flag.Usage = printUsage
	flag.Parse()

	if *showVersion {
		fmt.Printf("veloctl %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildTime)
		return
	}

	args := flag.Args()
	if len(args) == 0 {
		printUsage()
		os.Exit(1)
	}

	tok := *token
	if tok == "" {
		tok = loadSavedToken()
	}

	cfg := &cliConfig{
		endpoint:   strings.TrimRight(*endpoint, "/"),
		token:      tok,
		insecure:   *insecure,
		jsonOutput: *jsonOut,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				TLSClientConfig: &tls.Config{InsecureSkipVerify: *insecure}, //nolint:gosec
			},
		},
	}

	if err := dispatch(cfg, args); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func dispatch(cfg *cliConfig, args []string) error {
	cmd := args[0]
	sub := args[1:]

	switch cmd {
	case "version":
		fmt.Printf("veloctl %s (commit %s, built %s)\n", version.Version, version.Commit, version.BuildTime)
		return nil
	case "login":
		return runLogin(cfg, sub)
	case "user":
		return runUser(cfg, sub)
	case "token":
		return runToken(cfg, sub)
	case "system":
		return runSystem(cfg, sub)
	case "disk":
		return runDisk(cfg, sub)
	case "pool":
		return runPool(cfg, sub)
	case "volume":
		return runVolume(cfg, sub)
	case "snapshot":
		return runSnapshot(cfg, sub)
	case "portal":
		return runPortal(cfg, sub)
	case "host":
		return runHost(cfg, sub)
	case "initiator-group":
		return runInitiatorGroup(cfg, sub)
	case "export":
		return runExport(cfg, sub)
	default:
		return fmt.Errorf("unknown command %q (run 'veloctl -h' for usage)", cmd)
	}
}

func runLogin(cfg *cliConfig, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	username := fs.String("username", "admin", "Administrator username")
	password := fs.String("password", "", "Administrator password")
	_ = fs.Parse(args)

	if *password == "" {
		return fmt.Errorf("--password is required")
	}

	resp, err := cfg.request("POST", "/api/v1/auth/login", map[string]any{
		"username": *username,
		"password": *password,
	})
	if err != nil {
		return err
	}
	if cfg.jsonOutput {
		return printJSON(resp)
	}
	tok, _ := resp["access_token"].(string)
	if err := saveToken(tok); err != nil {
		return fmt.Errorf("save token: %w", err)
	}
	fmt.Printf("Logged in as %s (role: %v). Session token saved to %s\n", *username, resp["role"], tokenFilePath())
	return nil
}

func runUser(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | create | update | delete")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/auth/users", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
		_, _ = fmt.Fprintln(w, "USERNAME\tROLE\tUPDATED")
		for _, item := range sliceOfMaps(resp["users"]) {
			_, _ = fmt.Fprintf(w, "%v\t%v\t%v\n", item["username"], item["role"], item["updated_at"])
		}
		return w.Flush()

	case "create":
		fs := flag.NewFlagSet("user create", flag.ExitOnError)
		username := fs.String("username", "", "Username")
		password := fs.String("password", "", "Password (min 8 chars)")
		role := fs.String("role", "READONLY", "Role: ADMIN | CSI_PROVISIONER | READONLY")
		_ = fs.Parse(args[1:])
		if *username == "" || *password == "" {
			return fmt.Errorf("--username and --password are required")
		}
		resp, err := cfg.request("POST", "/api/v1/auth/users", map[string]any{
			"username": *username,
			"password": *password,
			"role":     strings.ToUpper(*role),
		})
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		fmt.Printf("Created user %q with role %s\n", *username, strings.ToUpper(*role))
		return nil

	case "update":
		fs := flag.NewFlagSet("user update", flag.ExitOnError)
		username := fs.String("username", "", "Username")
		role := fs.String("role", "", "New role: ADMIN | CSI_PROVISIONER | READONLY")
		password := fs.String("password", "", "Optional new password (min 8 chars)")
		_ = fs.Parse(args[1:])
		if *username == "" {
			return fmt.Errorf("--username is required")
		}
		body := map[string]any{}
		if *role != "" {
			body["role"] = strings.ToUpper(*role)
		}
		if *password != "" {
			body["password"] = *password
		}
		resp, err := cfg.request("PATCH", "/api/v1/auth/users/"+*username, body)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		fmt.Printf("Updated user %q\n", *username)
		return nil

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl user delete <username>")
		}
		_, err := cfg.request("DELETE", "/api/v1/auth/users/"+args[1], nil)
		if err != nil {
			return err
		}
		fmt.Printf("Deleted user %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown user subcommand %q", args[0])
	}
}

func runToken(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | create | revoke")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/auth/tokens", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tPREFIX\tROLE\tCREATED")
		for _, item := range sliceOfMaps(resp["tokens"]) {
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v\n",
				item["id"], item["name"], item["prefix"], item["role"], item["created_at"])
		}
		return tw.Flush()

	case "create":
		fs := flag.NewFlagSet("token create", flag.ExitOnError)
		name := fs.String("name", "", "Token name (e.g. k8s-csi)")
		role := fs.String("role", "CSI_PROVISIONER", "Role: ADMIN | CSI_PROVISIONER | READONLY")
		ttl := fs.Int64("ttl-seconds", 0, "Optional TTL in seconds (0 for no expiry)")
		_ = fs.Parse(args[1:])
		if *name == "" {
			return fmt.Errorf("--name is required")
		}
		resp, err := cfg.request("POST", "/api/v1/auth/tokens", map[string]any{
			"name":        *name,
			"role":        strings.ToUpper(*role),
			"ttl_seconds": *ttl,
		})
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		fmt.Printf("Created token %q:\nSecret Token (save now, shown once): %v\n", *name, resp["secret_token"])
		return nil

	case "revoke":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl token revoke <id>")
		}
		_, err := cfg.request("DELETE", "/api/v1/auth/tokens/"+args[1], nil)
		if err != nil {
			return err
		}
		fmt.Printf("Revoked token %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}

func runSystem(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: status | reconcile | iostats | upgrade | rollback")
	}
	switch args[0] {
	case "status":
		resp, err := cfg.request("GET", "/api/v1/system/status", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		node, _ := resp["node"].(map[string]any)
		spdkSt, _ := resp["spdk"].(map[string]any)
		recSt, _ := resp["reconciler"].(map[string]any)
		fmt.Printf("Overall Health:     %v\n", resp["overall_health"])
		fmt.Printf("Node ID / Hostname: %v (%v)\n", node["node_id"], node["hostname"])
		fmt.Printf("Velocor Version:    %v (commit %v)\n", node["version"], node["commit"])
		fmt.Printf("SPDK Connected:     %v (version %v, socket %v)\n", spdkSt["connected"], node["spdk_version"], spdkSt["socket_path"])
		fmt.Printf("Hugepages (MiB):    %v total / %v free\n", spdkSt["hugepages_total_mb"], spdkSt["hugepages_free_mb"])
		fmt.Printf("Drift Repaired:     %v\n", recSt["drift_repaired_count"])
		return nil

	case "reconcile":
		resp, err := cfg.request("POST", "/api/v1/system/reconcile", map[string]any{})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "iostats":
		resp, err := cfg.request("GET", "/api/v1/system/iostats", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "BDEV\tREAD OPS\tWRITE OPS\tBYTES READ\tBYTES WRITTEN")
		for _, item := range sliceOfMaps(resp["stats"]) {
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v\n",
				item["name"], item["num_read_ops"], item["num_write_ops"], item["bytes_read"], item["bytes_written"])
		}
		return tw.Flush()

	case "upgrade":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl system upgrade <oci-image-ref>")
		}
		resp, err := cfg.request("POST", "/api/v1/system/upgrade", map[string]any{"image_ref": args[1]})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "rollback":
		resp, err := cfg.request("POST", "/api/v1/system/rollback", map[string]any{})
		if err != nil {
			return err
		}
		return printJSON(resp)

	default:
		return fmt.Errorf("unknown system subcommand %q", args[0])
	}
}

func runDisk(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | scan | claim | release")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/disks", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tTYPE\tPCI/DEV\tSIZE(GiB)\tDRIVER\tBOOT\tCLAIMED\tPOOL\tBDEV")
		for _, d := range sliceOfMaps(resp["disks"]) {
			loc := fmt.Sprintf("%v", d["pci_address"])
			if loc == "" || loc == "<nil>" {
				loc = fmt.Sprintf("%v", d["device_path"])
			}
			szGiB := uint64FromAny(d["size_bytes"]) / (1024 * 1024 * 1024)
			fmt.Fprintf(tw, "%v\t%v\t%v\t%s\t%d\t%v\t%v\t%v\t%v\t%v\n",
				d["id"], d["name"], d["type"], loc, szGiB, d["driver_state"],
				d["is_boot_device"], d["claimed"], strOrEmpty(d["pool_id"]), strOrEmpty(d["spdk_bdev_name"]))
		}
		return tw.Flush()

	case "scan":
		resp, err := cfg.request("POST", "/api/v1/disks:scan", map[string]any{})
		if err != nil {
			return err
		}
		fmt.Printf("Scanned %d physical disk(s)\n", len(sliceOfMaps(resp["disks"])))
		return nil

	case "claim":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl disk claim <disk-id>")
		}
		resp, err := cfg.request("POST", "/api/v1/disks/"+args[1]+":claim", map[string]any{})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "release":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl disk release <disk-id>")
		}
		resp, err := cfg.request("POST", "/api/v1/disks/"+args[1]+":release", map[string]any{})
		if err != nil {
			return err
		}
		return printJSON(resp)

	default:
		return fmt.Errorf("unknown disk subcommand %q", args[0])
	}
}

func runPool(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | create | delete")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/pools", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tRAID\tCLUSTER(B)\tTOTAL(GiB)\tFREE(GiB)\tLVS\tHEALTH")
		for _, p := range sliceOfMaps(resp["pools"]) {
			totGiB := uint64FromAny(p["total_bytes"]) / (1024 * 1024 * 1024)
			freeGiB := uint64FromAny(p["free_bytes"]) / (1024 * 1024 * 1024)
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%d\t%d\t%v\t%v\n",
				p["id"], p["name"], p["raid_level"], p["cluster_size_bytes"],
				totGiB, freeGiB, p["lvs_name"], p["health_status"])
		}
		return tw.Flush()

	case "create":
		fs := flag.NewFlagSet("pool create", flag.ExitOnError)
		name := fs.String("name", "", "Pool name")
		raid := fs.String("raid", "RAID0", "RAID level: RAID0 | RAID1 | RAID10 | RAID5F")
		disksCSV := fs.String("disks", "", "Comma-separated disk IDs")
		clusterSz := fs.Uint("cluster-size", 65536, "SPDK lvol_store cluster size in bytes (default 64 KiB)")
		_ = fs.Parse(args[1:])
		if *name == "" || *disksCSV == "" {
			return fmt.Errorf("--name and --disks are required")
		}
		resp, err := cfg.request("POST", "/api/v1/pools", map[string]any{
			"name":               *name,
			"raid_level":         strings.ToUpper(*raid),
			"disk_ids":           strings.Split(*disksCSV, ","),
			"cluster_size_bytes": *clusterSz,
		})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl pool delete <pool-id>")
		}
		_, err := cfg.request("DELETE", "/api/v1/pools/"+args[1], nil)
		if err != nil {
			return err
		}
		fmt.Printf("Deleted pool %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown pool subcommand %q", args[0])
	}
}

func runVolume(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | create | resize | qos | delete")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/volumes", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tPOOL\tSIZE(GiB)\tTHIN\tATTACHMENT\tBDEV\tHEALTH")
		for _, v := range sliceOfMaps(resp["volumes"]) {
			szGiB := uint64FromAny(v["size_bytes"]) / (1024 * 1024 * 1024)
			fmt.Fprintf(tw, "%v\t%v\t%v\t%d\t%v\t%v\t%v\t%v\n",
				v["id"], v["name"], v["pool_id"], szGiB, v["thin_provision"],
				v["attachment_state"], v["effective_bdev_name"], v["health_status"])
		}
		return tw.Flush()

	case "create":
		fs := flag.NewFlagSet("volume create", flag.ExitOnError)
		name := fs.String("name", "", "Volume name")
		poolID := fs.String("pool", "", "Pool ID")
		sizeGiB := fs.Uint64("size-gib", 10, "Volume size in GiB")
		thin := fs.Bool("thin", true, "Thin provision volume")
		_ = fs.Parse(args[1:])
		if *name == "" || *poolID == "" {
			return fmt.Errorf("--name and --pool are required")
		}
		resp, err := cfg.request("POST", "/api/v1/volumes", map[string]any{
			"name":           *name,
			"pool_id":        *poolID,
			"size_bytes":     *sizeGiB * 1024 * 1024 * 1024,
			"thin_provision": *thin,
		})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "resize":
		fs := flag.NewFlagSet("volume resize", flag.ExitOnError)
		id := fs.String("id", "", "Volume ID")
		sizeGiB := fs.Uint64("size-gib", 0, "New volume size in GiB")
		_ = fs.Parse(args[1:])
		if *id == "" || *sizeGiB == 0 {
			return fmt.Errorf("--id and --size-gib are required")
		}
		resp, err := cfg.request("POST", "/api/v1/volumes/"+*id+":resize", map[string]any{
			"new_size_bytes": *sizeGiB * 1024 * 1024 * 1024,
		})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "qos":
		fs := flag.NewFlagSet("volume qos", flag.ExitOnError)
		id := fs.String("id", "", "Volume ID")
		iops := fs.Uint64("rw-iops", 0, "Max RW IOPS (0 for unlimited)")
		mbps := fs.Uint64("rw-mbps", 0, "Max RW MiB/s (0 for unlimited)")
		_ = fs.Parse(args[1:])
		if *id == "" {
			return fmt.Errorf("--id is required")
		}
		resp, err := cfg.request("PUT", "/api/v1/volumes/"+*id+"/qos", map[string]any{
			"qos_policy": map[string]any{
				"rw_ios_per_sec":    *iops,
				"rw_mbytes_per_sec": *mbps,
			},
		})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl volume delete <volume-id>")
		}
		_, err := cfg.request("DELETE", "/api/v1/volumes/"+args[1], nil)
		if err != nil {
			return err
		}
		fmt.Printf("Deleted volume %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown volume subcommand %q", args[0])
	}
}

func runSnapshot(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | create | clone | allocated-blocks | changed-blocks | delete")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/snapshots", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tVOLUME\tSEQ\tPARENT\tCREATED")
		for _, s := range sliceOfMaps(resp["snapshots"]) {
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v\t%v\n",
				s["id"], s["name"], s["volume_id"], s["sequence_number"],
				strOrEmpty(s["parent_snapshot_id"]), s["created_at"])
		}
		return tw.Flush()

	case "create":
		fs := flag.NewFlagSet("snapshot create", flag.ExitOnError)
		volID := fs.String("volume", "", "Volume ID")
		name := fs.String("name", "", "Snapshot name")
		_ = fs.Parse(args[1:])
		if *volID == "" || *name == "" {
			return fmt.Errorf("--volume and --name are required")
		}
		resp, err := cfg.request("POST", "/api/v1/volumes/"+*volID+"/snapshots", map[string]any{"name": *name})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "clone":
		fs := flag.NewFlagSet("snapshot clone", flag.ExitOnError)
		snapID := fs.String("snapshot", "", "Snapshot ID")
		name := fs.String("name", "", "Clone volume name")
		_ = fs.Parse(args[1:])
		if *snapID == "" || *name == "" {
			return fmt.Errorf("--snapshot and --name are required")
		}
		resp, err := cfg.request("POST", "/api/v1/snapshots/"+*snapID+":clone", map[string]any{"clone_name": *name})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "allocated-blocks":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl snapshot allocated-blocks <snapshot-id>")
		}
		resp, err := cfg.request("GET", "/api/v1/snapshots/"+args[1]+"/blocks:allocated", nil)
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "changed-blocks":
		fs := flag.NewFlagSet("snapshot changed-blocks", flag.ExitOnError)
		baseID := fs.String("base", "", "Base snapshot ID")
		targetID := fs.String("target", "", "Target snapshot ID")
		_ = fs.Parse(args[1:])
		if *baseID == "" || *targetID == "" {
			return fmt.Errorf("--base and --target snapshot IDs are required")
		}
		path := fmt.Sprintf("/api/v1/snapshots/%s/blocks:changed?base_snapshot_id=%s",
			url.PathEscape(*targetID), url.QueryEscape(*baseID))
		resp, err := cfg.request("GET", path, nil)
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl snapshot delete <snapshot-id>")
		}
		_, err := cfg.request("DELETE", "/api/v1/snapshots/"+args[1], nil)
		if err != nil {
			return err
		}
		fmt.Printf("Deleted snapshot %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown snapshot subcommand %q", args[0])
	}
}

func runPortal(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | create | delete")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/portals", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tPROTOCOL\tIP\tPORT")
		for _, p := range sliceOfMaps(resp["portals"]) {
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v\n",
				p["id"], p["name"], p["protocol"], p["ip_address"], p["port"])
		}
		return tw.Flush()

	case "create":
		fs := flag.NewFlagSet("portal create", flag.ExitOnError)
		name := fs.String("name", "", "Portal name")
		proto := fs.String("protocol", "NVMEOF_TCP", "Protocol: NVMEOF_TCP | NVMEOF_RDMA | ISCSI")
		ip := fs.String("ip", "", "Listen IP address")
		port := fs.Uint("port", 4420, "Listen TCP/RDMA port")
		_ = fs.Parse(args[1:])
		if *name == "" || *ip == "" {
			return fmt.Errorf("--name and --ip are required")
		}
		resp, err := cfg.request("POST", "/api/v1/portals", map[string]any{
			"name":       *name,
			"protocol":   strings.ToUpper(*proto),
			"ip_address": *ip,
			"port":       *port,
		})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl portal delete <portal-id>")
		}
		_, err := cfg.request("DELETE", "/api/v1/portals/"+args[1], nil)
		if err != nil {
			return err
		}
		fmt.Printf("Deleted portal %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown portal subcommand %q", args[0])
	}
}

func runHost(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | create | delete")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/hosts", nil)
		if err != nil {
			return err
		}
		return printJSON(resp)
	case "create":
		fs := flag.NewFlagSet("host create", flag.ExitOnError)
		name := fs.String("name", "", "Host name")
		nqn := fs.String("nqn", "", "NVMe-oF Host NQN")
		iqn := fs.String("iqn", "", "iSCSI Initiator IQN")
		ip := fs.String("ip", "", "Host IP address or CIDR")
		_ = fs.Parse(args[1:])
		if *name == "" {
			return fmt.Errorf("--name is required")
		}
		resp, err := cfg.request("POST", "/api/v1/hosts", map[string]any{
			"name":          *name,
			"host_nqn":      *nqn,
			"initiator_iqn": *iqn,
			"ip_address":    *ip,
		})
		if err != nil {
			return err
		}
		return printJSON(resp)
	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl host delete <id>")
		}
		if _, err := cfg.request("DELETE", "/api/v1/hosts/"+args[1], nil); err != nil {
			return err
		}
		fmt.Printf("Deleted host %s\n", args[1])
		return nil
	default:
		return fmt.Errorf("unknown host subcommand %q", args[0])
	}
}

func runInitiatorGroup(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | create | delete")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/initiator-groups", nil)
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "create":
		fs := flag.NewFlagSet("initiator-group create", flag.ExitOnError)
		name := fs.String("name", "", "Initiator group name")
		hostIDs := fs.String("host-ids", "", "Comma-separated Host IDs")
		nqns := fs.String("nqns", "", "Comma-separated NVMe-oF Host NQNs")
		iqns := fs.String("iqns", "", "Comma-separated iSCSI Initiator IQNs")
		_ = fs.Parse(args[1:])
		if *name == "" {
			return fmt.Errorf("--name is required")
		}
		var hostIDList, nqnList, iqnList []string
		if *hostIDs != "" {
			hostIDList = strings.Split(*hostIDs, ",")
		}
		if *nqns != "" {
			nqnList = strings.Split(*nqns, ",")
		}
		if *iqns != "" {
			iqnList = strings.Split(*iqns, ",")
		}
		resp, err := cfg.request("POST", "/api/v1/initiator-groups", map[string]any{
			"name":           *name,
			"host_ids":       hostIDList,
			"host_nqns":      nqnList,
			"initiator_iqns": iqnList,
		})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl initiator-group delete <id>")
		}
		_, err := cfg.request("DELETE", "/api/v1/initiator-groups/"+args[1], nil)
		if err != nil {
			return err
		}
		fmt.Printf("Deleted initiator group %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown initiator-group subcommand %q", args[0])
	}
}

func runExport(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | create | delete")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/exports", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tVOLUME\tPROTOCOL\tNQN/IQN\tHEALTH")
		for _, e := range sliceOfMaps(resp["exports"]) {
			target := strOrEmpty(e["subsystem_nqn"])
			if target == "" {
				target = strOrEmpty(e["target_iqn"])
			}
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%s\t%v\n",
				e["id"], e["name"], e["volume_id"], e["protocol"], target, e["health_status"])
		}
		return tw.Flush()

	case "create":
		fs := flag.NewFlagSet("export create", flag.ExitOnError)
		name := fs.String("name", "", "Export name")
		volID := fs.String("volume", "", "Volume ID")
		proto := fs.String("protocol", "NVMEOF_TCP", "Protocol: NVMEOF_TCP | NVMEOF_RDMA | ISCSI")
		portals := fs.String("portals", "", "Comma-separated Portal IDs")
		igs := fs.String("initiator-groups", "", "Comma-separated InitiatorGroup IDs")
		allowAny := fs.Bool("allow-any-host", false, "Allow any host NQN/IQN")
		_ = fs.Parse(args[1:])
		if *name == "" || *volID == "" {
			return fmt.Errorf("--name and --volume are required")
		}
		var portalList, igList []string
		if *portals != "" {
			portalList = strings.Split(*portals, ",")
		}
		if *igs != "" {
			igList = strings.Split(*igs, ",")
		}
		resp, err := cfg.request("POST", "/api/v1/exports", map[string]any{
			"name":                *name,
			"volume_id":           *volID,
			"protocol":            strings.ToUpper(*proto),
			"portal_ids":          portalList,
			"initiator_group_ids": igList,
			"allow_any_host":      *allowAny,
		})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl export delete <export-id>")
		}
		_, err := cfg.request("DELETE", "/api/v1/exports/"+args[1], nil)
		if err != nil {
			return err
		}
		fmt.Printf("Deleted export %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown export subcommand %q", args[0])
	}
}

func (c *cliConfig) request(method, path string, body any) (map[string]any, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, c.endpoint+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	if resp.StatusCode >= 400 {
		if errMsg, ok := out["error"].(string); ok {
			return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, errMsg)
		}
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return out, nil
}

func printUsage() {
	fmt.Fprintf(os.Stderr, `veloctl - CLI for Velocor SPDK All-Flash SAN/NAS Appliances

Usage:
  veloctl [global-flags] <command> <subcommand> [flags]

Commands:
  login             Authenticate admin session and cache JWT
  token             Manage scoped service tokens (list, create, revoke)
  system            Appliance status, reconcile, iostats, bootc upgrade/rollback
  disk              List, scan, claim, or release NVMe and SSD drives
  pool              Create, list, or delete SPDK RAID + lvol_store pools
  volume            Create, list, resize, set QoS, or delete logical volumes
  snapshot          Create, list, clone, delete snapshots & query CBT blocks
  portal            Manage NVMe-oF and iSCSI network portals
  initiator-group   Manage host NQN/IQN ACL initiator groups
  export            Create, list, or delete NVMe-oF and iSCSI volume exports
  version           Print version information

Global Flags:
`)
	flag.PrintDefaults()
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newTabWriter() *tabwriter.Writer {
	return tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
}

func sliceOfMaps(v any) []map[string]any {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

func uint64FromAny(v any) uint64 {
	switch val := v.(type) {
	case float64:
		return uint64(val)
	case string:
		n, _ := strconv.ParseUint(val, 10, 64)
		return n
	default:
		return 0
	}
}

func strOrEmpty(v any) string {
	if v == nil {
		return ""
	}
	return fmt.Sprintf("%v", v)
}

func tokenFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "velocor", "token")
}

func saveToken(tok string) error {
	p := tokenFilePath()
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(tok+"\n"), 0600)
}

func loadSavedToken() string {
	data, err := os.ReadFile(tokenFilePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func envOrDefault(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
