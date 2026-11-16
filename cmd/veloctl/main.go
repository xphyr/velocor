package main

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"crypto/x509"
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
	"golang.org/x/sys/unix"
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
		insecure    = flag.Bool("insecure", os.Getenv("VELOCOR_INSECURE") == "true", "Skip TLS certificate verification (exposes your session to anyone on the network path; lab use only)")
		caCert      = flag.String("ca-cert", os.Getenv("VELOCOR_CA_CERT"), "Certificate to trust for the appliance: a PEM file path, or PEM text (e.g. a copy of /var/lib/velocor/tls.crt)")
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

	tlsCfg, err := tlsConfig(*insecure, *caCert)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
	cfg := &cliConfig{
		endpoint:   strings.TrimRight(*endpoint, "/"),
		token:      tok,
		insecure:   *insecure,
		jsonOutput: *jsonOut,
		httpClient: &http.Client{
			Timeout:   30 * time.Second,
			Transport: &http.Transport{TLSClientConfig: tlsCfg},
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
	case "passwd", "password":
		return runPasswd(cfg, sub)
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
	case "group-snapshot", "gsnap", "vgs":
		return runGroupSnapshot(cfg, sub)
	default:
		return fmt.Errorf("unknown command %q (run 'veloctl -h' for usage)", cmd)
	}
}

func runLogin(cfg *cliConfig, args []string) error {
	fs := flag.NewFlagSet("login", flag.ExitOnError)
	username := fs.String("username", "admin", "Username")
	password := fs.String("password", "", "Password (prompted for, without echo, if omitted)")
	_ = fs.Parse(args)

	if *password == "" {
		p, err := promptSecret("Password for " + *username + ": ")
		if err != nil {
			return err
		}
		*password = p
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
	if mc, _ := resp["must_change_password"].(bool); mc {
		fmt.Println("This account must choose a new password before anything else: run 'veloctl passwd'.")
	}
	return nil
}

// runPasswd changes the signed-in user's own password. The server revokes every earlier
// session, so the fresh token it returns replaces the saved one.
func runPasswd(cfg *cliConfig, args []string) error {
	fs := flag.NewFlagSet("passwd", flag.ExitOnError)
	current := fs.String("current", "", "Current password (prompted for if omitted)")
	next := fs.String("new", "", "New password, at least 8 characters (prompted for if omitted)")
	_ = fs.Parse(args)
	var err error
	if *current == "" {
		if *current, err = promptSecret("Current password: "); err != nil {
			return err
		}
	}
	if *next == "" {
		if *next, err = promptSecret("New password: "); err != nil {
			return err
		}
		again, err := promptSecret("New password again: ")
		if err != nil {
			return err
		}
		if again != *next {
			return fmt.Errorf("the new passwords don't match")
		}
	}
	resp, err := cfg.request("POST", "/api/v1/auth/password", map[string]any{"current_password": *current, "new_password": *next})
	if err != nil {
		return err
	}
	if tok, _ := resp["access_token"].(string); tok != "" {
		if err := saveToken(tok); err != nil {
			return fmt.Errorf("password changed, but saving the new session failed: %w", err)
		}
	}
	fmt.Println("Password changed. Your other sessions have ended.")
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
		return fmt.Errorf("subcommand required: status | os | reconcile | iostats | upgrade | rollback | reboot | containers")
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
		if degraded, _ := recSt["degraded_resources"].([]any); len(degraded) > 0 {
			fmt.Printf("Needing Attention:  %v\n", degraded)
		}
		for _, w := range sliceOfAny(resp["warnings"]) {
			fmt.Printf("WARNING: %v\n", w)
		}
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
		imageRef := ""
		if len(args) >= 2 {
			imageRef = args[1]
		}
		resp, err := cfg.request("POST", "/api/v1/system/upgrade", map[string]any{"image_ref": imageRef})
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

	case "os":
		resp, err := cfg.request("GET", "/api/v1/system/os", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		fmt.Printf("Bootc Available: %v\n", resp["available"])
		if msg, ok := resp["message"].(string); ok && msg != "" {
			fmt.Printf("Message:         %s\n", msg)
		}
		for _, slot := range []string{"booted", "staged", "rollback"} {
			if m, ok := resp[slot].(map[string]any); ok {
				fmt.Printf("%-16s %v (version=%v digest=%v)\n", strings.ToUpper(slot)+":", m["image"], m["version"], m["image_digest"])
			}
		}
		return nil

	case "reboot":
		resp, err := cfg.request("POST", "/api/v1/system/reboot", map[string]any{})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "containers":
		resp, err := cfg.request("GET", "/api/v1/system/containers", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		if msg, _ := resp["message"].(string); msg != "" {
			fmt.Println(msg)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "UNIT\tIMAGE\tSTATUS")
		for _, c := range sliceOfMaps(resp["containers"]) {
			fmt.Fprintf(tw, "%v\t%v\t%v\n", c["unit"], c["image"], c["running_status"])
		}
		return tw.Flush()

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
		fs := flag.NewFlagSet("disk claim", flag.ExitOnError)
		force := fs.Bool("force", false, "Claim even if the disk has partitions or filesystem signatures (DESTROYS its data)")
		_ = fs.Parse(args[1:])
		if fs.NArg() < 1 {
			return fmt.Errorf("usage: veloctl disk claim [--force] <disk-id>")
		}
		path := "/api/v1/disks/" + url.PathEscape(fs.Arg(0)) + ":claim"
		if *force {
			path += "?force=true"
		}
		resp, err := cfg.request("POST", path, map[string]any{})
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
		fmt.Fprintln(tw, "ID\tNAME\tRAID\tCLUSTER(B)\tTOTAL(GiB)\tFREE(GiB)\tLVS\tHEALTH\tDETAILS")
		for _, p := range sliceOfMaps(resp["pools"]) {
			totGiB := uint64FromAny(p["total_bytes"]) / (1024 * 1024 * 1024)
			freeGiB := uint64FromAny(p["free_bytes"]) / (1024 * 1024 * 1024)
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%d\t%d\t%v\t%v\t%v\n",
				p["id"], p["name"], p["raid_level"], p["cluster_size_bytes"],
				totGiB, freeGiB, p["lvs_name"], p["health_status"], strOrEmpty(p["status_message"]))
		}
		return tw.Flush()

	case "create":
		fs := flag.NewFlagSet("pool create", flag.ExitOnError)
		name := fs.String("name", "", "Pool name")
		raid := fs.String("raid", "RAID0", "RAID level: RAID0 | RAID1 | RAID10")
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
		return fmt.Errorf("subcommand required: list | get | usage | create | resize | qos | delete")
	}
	switch args[0] {
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl volume get <volume-id>")
		}
		resp, err := cfg.request("GET", "/api/v1/volumes/"+url.PathEscape(args[1]), nil)
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "usage":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl volume usage <volume-id>")
		}
		resp, err := cfg.request("GET", "/api/v1/volumes/"+url.PathEscape(args[1])+"/usage", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		size, alloc := uint64FromAny(resp["size_bytes"]), uint64FromAny(resp["allocated_bytes"])
		pct := 0.0
		if size > 0 {
			pct = 100 * float64(alloc) / float64(size)
		}
		fmt.Printf("Allocated: %s of %s (%.1f%%), thin=%v\n", humanBytes(alloc), humanBytes(size), pct, resp["thin_provision"])
		return nil

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
		return fmt.Errorf("subcommand required: list | create | clone | delete")
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

func runGroupSnapshot(cfg *cliConfig, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("subcommand required: list | get | create | delete")
	}
	switch args[0] {
	case "list":
		resp, err := cfg.request("GET", "/api/v1/volume-group-snapshots", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tVOLUMES\tSNAPSHOTS\tREADY\tCREATED")
		for _, g := range sliceOfMaps(resp["volume_group_snapshots"]) {
			vols, _ := g["volume_ids"].([]any)
			snaps, _ := g["snapshot_ids"].([]any)
			fmt.Fprintf(tw, "%v\t%v\t%d\t%d\t%v\t%v\n",
				g["id"], g["name"], len(vols), len(snaps), g["ready_to_use"], g["created_at"])
		}
		return tw.Flush()

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl group-snapshot get <group-snapshot-id>")
		}
		resp, err := cfg.request("GET", "/api/v1/volume-group-snapshots/"+args[1], nil)
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "create":
		fs := flag.NewFlagSet("group-snapshot create", flag.ExitOnError)
		name := fs.String("name", "", "Group snapshot name")
		volumesCSV := fs.String("volumes", "", "Comma-separated volume IDs")
		_ = fs.Parse(args[1:])
		if *name == "" || *volumesCSV == "" {
			return fmt.Errorf("--name and --volumes are required")
		}
		var volIDs []string
		for _, v := range strings.Split(*volumesCSV, ",") {
			if trimmed := strings.TrimSpace(v); trimmed != "" {
				volIDs = append(volIDs, trimmed)
			}
		}
		resp, err := cfg.request("POST", "/api/v1/volume-group-snapshots", map[string]any{
			"name":       *name,
			"volume_ids": volIDs,
		})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "delete":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl group-snapshot delete <group-snapshot-id>")
		}
		_, err := cfg.request("DELETE", "/api/v1/volume-group-snapshots/"+args[1], nil)
		if err != nil {
			return err
		}
		fmt.Printf("Deleted volume group snapshot %s\n", args[1])
		return nil

	default:
		return fmt.Errorf("unknown group-snapshot subcommand %q", args[0])
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
		return fmt.Errorf("subcommand required: list | create | update | delete")
	}
	switch args[0] {
	case "update":
		fs := flag.NewFlagSet("initiator-group update", flag.ExitOnError)
		hostIDs := fs.String("host-ids", "", "Comma-separated Host IDs (replaces the current list)")
		nqns := fs.String("nqns", "", "Comma-separated NVMe-oF host NQNs (replaces the current list)")
		iqns := fs.String("iqns", "", "Comma-separated iSCSI initiator IQNs (replaces the current list)")
		cidrs := fs.String("cidrs", "", "Comma-separated iSCSI source CIDRs (replaces the current list)")
		id, err := parseWithID(fs, args[1:])
		if err != nil {
			return fmt.Errorf("usage: veloctl initiator-group update <id> [--host-ids ...] [--nqns ...] [--iqns ...] [--cidrs ...]")
		}
		// Only lists that were given are sent, so the others are left as they are.
		body := map[string]any{}
		fields := map[string]string{"host-ids": "host_ids", "nqns": "host_nqns", "iqns": "initiator_iqns", "cidrs": "allowed_cidrs"}
		values := map[string]*string{"host-ids": hostIDs, "nqns": nqns, "iqns": iqns, "cidrs": cidrs}
		fs.Visit(func(f *flag.Flag) { body[fields[f.Name]] = splitList(*values[f.Name]) })
		if len(body) == 0 {
			return fmt.Errorf("nothing to change: give at least one of --host-ids, --nqns, --iqns, --cidrs")
		}
		resp, err := cfg.request("PATCH", "/api/v1/initiator-groups/"+url.PathEscape(id), body)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		fmt.Printf("Updated initiator group %s; re-applied to %v export(s)\n", id, resp["exports_updated"])
		return nil

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
		cidrs := fs.String("cidrs", "", "Comma-separated iSCSI source CIDRs")
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
			"allowed_cidrs":  splitList(*cidrs),
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
		return fmt.Errorf("subcommand required: list | get | connections | access | create | delete")
	}
	switch args[0] {
	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl export get <export-id>")
		}
		resp, err := cfg.request("GET", "/api/v1/exports/"+url.PathEscape(args[1]), nil)
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "connections":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl export connections <export-id>")
		}
		resp, err := cfg.request("GET", "/api/v1/exports/"+url.PathEscape(args[1])+"/connections", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		conns := sliceOfMaps(resp["connections"])
		if len(conns) == 0 {
			fmt.Println("Nothing is connected.")
			return nil
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "HOST\tADDRESS\tQUEUES")
		for _, c := range conns {
			fmt.Fprintf(tw, "%s\t%s\t%v\n", strOrDash(c["host"]), strOrDash(c["address"]), c["queues"])
		}
		return tw.Flush()

	case "access":
		fs := flag.NewFlagSet("export access", flag.ExitOnError)
		igs := fs.String("initiator-groups", "", "Comma-separated InitiatorGroup IDs allowed to connect (replaces the current list; empty for none)")
		allowAny := fs.Bool("allow-any-host", false, "Let any host connect, with no access control (--allow-any-host=false to require a list again)")
		id, err := parseWithID(fs, args[1:])
		if err != nil {
			return fmt.Errorf("usage: veloctl export access <export-id> [--initiator-groups a,b] [--allow-any-host=true|false]")
		}
		body := map[string]any{}
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "initiator-groups":
				body["initiator_group_ids"] = splitList(*igs)
			case "allow-any-host":
				body["allow_any_host"] = *allowAny
			}
		})
		if len(body) == 0 {
			return fmt.Errorf("nothing to change: give --initiator-groups and/or --allow-any-host")
		}
		resp, err := cfg.request("PATCH", "/api/v1/exports/"+url.PathEscape(id), body)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		exp, _ := resp["export"].(map[string]any)
		if open, _ := exp["allow_any_host"].(bool); open {
			fmt.Printf("Export %s now lets ANY host connect.\n", id)
		} else {
			fmt.Printf("Export %s is limited to initiator groups %v.\n", id, exp["initiator_group_ids"])
		}
		return nil

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
			if strings.Contains(errMsg, "initial password must be changed") {
				errMsg += " (run 'veloctl passwd')"
			}
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
  login             Sign in and cache the session token
  passwd            Change your own password
  user              Manage local users (list, create, update, delete)
  token             Manage scoped service tokens (list, create, revoke)
  system            Appliance status, containers, bootc upgrade/rollback
  disk              List, scan, claim, or release NVMe and SSD drives
  pool              Create, list, or delete SPDK RAID + lvol_store pools
  volume            Create, list, get, show usage, resize, set QoS, or delete volumes
  snapshot          Create, list, clone and delete snapshots
  group-snapshot    Create, list, get, or delete consistency group snapshots
  portal            Manage NVMe-oF and iSCSI network portals
  host              Register NVMe-oF / iSCSI initiator hosts
  initiator-group   Manage host NQN/IQN ACL initiator groups (create, update, delete)
  export            Create, inspect, change access to, or delete volume exports
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

// tlsConfig verifies the appliance against the system roots plus caCert (a PEM file path or
// PEM text). insecure disables verification entirely and is only for labs.
func tlsConfig(insecure bool, caCert string) (*tls.Config, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, InsecureSkipVerify: insecure} //nolint:gosec // explicit opt-in only
	caCert = strings.TrimSpace(caCert)
	if caCert == "" {
		return cfg, nil
	}
	pemData := []byte(caCert)
	if !strings.HasPrefix(caCert, "-----BEGIN") {
		b, err := os.ReadFile(caCert)
		if err != nil {
			return nil, fmt.Errorf("read --ca-cert: %w", err)
		}
		pemData = b
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(pemData) {
		return nil, fmt.Errorf("--ca-cert does not contain a PEM certificate")
	}
	cfg.RootCAs = pool
	return cfg, nil
}

// promptSecret reads a line from the terminal without echoing it. When stdin isn't a
// terminal (a pipe in a script), it reads the line as is.
func promptSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd())
	old, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err == nil {
		noEcho := *old
		noEcho.Lflag &^= unix.ECHO
		if err := unix.IoctlSetTermios(fd, unix.TCSETS, &noEcho); err == nil {
			defer func() {
				_ = unix.IoctlSetTermios(fd, unix.TCSETS, old)
				fmt.Fprintln(os.Stderr)
			}()
		}
	}
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read password: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// parseWithID accepts "<id> [flags]" as well as "[flags] <id>".
func parseWithID(fs *flag.FlagSet, args []string) (string, error) {
	var id string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return "", err
	}
	if id == "" {
		id = fs.Arg(0)
	}
	if id == "" {
		return "", fmt.Errorf("an ID is required")
	}
	return id, nil
}

func splitList(s string) []string {
	out := []string{}
	for _, v := range strings.Split(s, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func sliceOfAny(v any) []any {
	s, _ := v.([]any)
	return s
}

func strOrDash(v any) string {
	if s := strOrEmpty(v); s != "" {
		return s
	}
	return "-"
}

func humanBytes(b uint64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := uint64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(b)/float64(div), "KMGTPE"[exp])
}
