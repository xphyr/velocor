package main

import (
	"bufio"
	"bytes"
	"context"
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
	"golang.org/x/term"
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
		// Per-request timeouts (see request): reads are quick, changes can take as long as
		// SPDK needs (a new pool unmaps whole disks).
		httpClient: &http.Client{Transport: &http.Transport{TLSClientConfig: tlsCfg}},
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
		return fmt.Errorf("subcommand required: status | os | reconcile | iostats | upgrade | rollback | reboot | containers | rebuild | scrub | volume-retention | monitoring | network | backup | backups | restore")
	}
	switch args[0] {
	case "backup", "backups", "restore":
		return runSystemBackup(cfg, args)
	case "rebuild":
		fs := flag.NewFlagSet("system rebuild", flag.ExitOnError)
		limit := fs.Int("max-mb-per-sec", -1, "Limit RAID rebuild bandwidth for every pool (0 = unlimited); applies to rebuilds that start afterwards. Omit to show the current limit")
		_ = fs.Parse(args[1:])
		var resp map[string]any
		var err error
		if *limit < 0 {
			resp, err = cfg.request("GET", "/api/v1/system/rebuild", nil)
		} else {
			resp, err = cfg.request("PUT", "/api/v1/system/rebuild", map[string]any{"max_mb_per_sec": *limit})
		}
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		if v := uint64FromAny(resp["max_mb_per_sec"]); v == 0 {
			fmt.Println("Rebuild speed: unlimited")
		} else {
			fmt.Printf("Rebuild speed: at most %d MB/s per pool\n", v)
		}
		return nil

	case "scrub":
		return runSystemScrub(cfg, args[1:])

	case "volume-retention":
		fs := flag.NewFlagSet("system volume-retention", flag.ExitOnError)
		hours := fs.Int("hours", -1, "Keep deleted volumes this many hours before purging them (0 = delete at once); applies to volumes deleted afterwards. Omit to show the current setting")
		_ = fs.Parse(args[1:])
		var resp map[string]any
		var err error
		if *hours < 0 {
			resp, err = cfg.request("GET", "/api/v1/system/volume-retention", nil)
		} else {
			resp, err = cfg.request("PUT", "/api/v1/system/volume-retention", map[string]any{"hours": *hours})
		}
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		if h := uint64FromAny(resp["hours"]); h == 0 {
			fmt.Println("Deleted volumes: deleted at once")
		} else {
			fmt.Printf("Deleted volumes: kept %d hours before they are purged\n", h)
		}
		return nil

	case "network":
		// veloctl system network [confirm|rollback <change-id>]
		if len(args) >= 3 && (args[1] == "confirm" || args[1] == "rollback") {
			resp, err := cfg.request("POST", "/api/v1/system/network/"+args[1], map[string]any{"id": args[2]})
			if err != nil {
				return err
			}
			if cfg.jsonOutput {
				return printJSON(resp)
			}
			c, _ := resp["change"].(map[string]any)
			fmt.Printf("Change %v to %v: %v\n", c["id"], c["interface"], c["state"])
			return nil
		}
		resp, err := cfg.request("GET", "/api/v1/system/network", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "INTERFACE\tLINK\tADDRESSING\tADDRESSES\tMTU")
		for _, i := range sliceOfMaps(resp["interfaces"]) {
			link := "down"
			if i["link_up"] == true {
				link = "up"
			}
			var addrs []string
			for _, a := range sliceOfAny(i["addresses"]) {
				addrs = append(addrs, fmt.Sprint(a))
			}
			fmt.Fprintf(tw, "%v\t%s\t%v\t%s\t%v\n", i["name"], link, i["method"], strings.Join(addrs, ","), i["mtu"])
		}
		_ = tw.Flush()
		if c, ok := resp["pending_change"].(map[string]any); ok {
			fmt.Printf("\nPENDING: change %v to %v, rolled back at %v unless confirmed:\n  veloctl system network confirm %v\n  veloctl system network rollback %v\n",
				c["id"], c["interface"], c["deadline"], c["id"], c["id"])
		} else if c, ok := resp["last_change"].(map[string]any); ok {
			fmt.Printf("\nLast change: %v to %v, %v\n", c["id"], c["interface"], c["state"])
		}
		return nil

	case "monitoring":
		fs := flag.NewFlagSet("system monitoring", flag.ExitOnError)
		on := fs.Bool("on", false, "Turn on the built-in Prometheus and Alertmanager")
		off := fs.Bool("off", false, "Turn them off (their data is kept)")
		_ = fs.Parse(args[1:])
		if *on && *off {
			return fmt.Errorf("give --on or --off, not both")
		}
		var resp map[string]any
		var err error
		switch {
		case *on || *off:
			resp, err = cfg.request("PUT", "/api/v1/system/monitoring", map[string]any{"enabled": *on})
		default:
			resp, err = cfg.request("GET", "/api/v1/system/monitoring", nil)
		}
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		if resp["available"] != true {
			fmt.Println("Built-in monitoring isn't available on this host (no /var/lib/velocor-monitoring).")
			return nil
		}
		m, _ := resp["monitoring"].(map[string]any)
		state := "off"
		if m["enabled"] == true {
			state = "on"
		}
		fmt.Printf("Built-in monitoring: %s (data kept %v days, at most %s)\n", state, m["retention_days"], humanBytes(uint64FromAny(m["retention_bytes"])))
		for _, svc := range []string{"prometheus", "alertmanager"} {
			s, _ := m[svc].(map[string]any)
			st := "stopped"
			switch {
			case s["ready"] == true:
				st = "running"
			case s["active"] == true:
				st = "starting"
			}
			if d, _ := s["detail"].(string); d != "" && st != "running" {
				st += " (" + d + ")"
			}
			fmt.Printf("  %-13s %s\n", svc+":", st)
		}
		return nil

	case "status":
		resp, err := cfg.request("GET", "/api/v1/system/status", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		node, _ := resp["node"].(map[string]any)
		spdkSt, _ := resp["data_plane"].(map[string]any)
		recSt, _ := resp["reconciler"].(map[string]any)
		fmt.Printf("Overall Health:     %v\n", resp["overall_health"])
		fmt.Printf("Node ID / Hostname: %v (%v)\n", node["node_id"], node["hostname"])
		fmt.Printf("Velocor Version:    %v (commit %v)\n", node["version"], node["commit"])
		fmt.Printf("Data plane:         connected %v (%v, %v)\n", spdkSt["connected"], node["data_plane_version"], spdkSt["endpoint"])
		fmt.Printf("Hugepages (MiB):    %v total / %v free\n", spdkSt["hugepages_total_mb"], spdkSt["hugepages_free_mb"])
		fmt.Printf("Drift Repaired:     %v\n", recSt["drift_repaired_count"])
		if degraded, _ := recSt["degraded_resources"].([]any); len(degraded) > 0 {
			fmt.Printf("Needing Attention:  %v\n", degraded)
		}
		for _, w := range sliceOfAny(resp["warnings"]) {
			fmt.Printf("WARNING: %v\n", w)
		}
		if objs := sliceOfMaps(recSt["unmanaged_objects"]); len(objs) > 0 {
			fmt.Println("\nIn spdk_tgt, not managed by Velocor (left alone):")
			tw := newTabWriter()
			fmt.Fprintln(tw, "KIND\tNAME\tUUID\tDETAILS\tSINCE")
			for _, o := range objs {
				fmt.Fprintf(tw, "%v\t%v\t%v\t%v\t%v\n", o["kind"], o["name"], strOrDash(o["uuid"]), strOrDash(o["detail"]), o["first_seen"])
			}
			return tw.Flush()
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
		// veloctl system upgrade [image] [--no-wait] | --status | --dismiss-warning
		fs := flag.NewFlagSet("system upgrade", flag.ExitOnError)
		noWait := fs.Bool("no-wait", false, "Start the upgrade and return; follow it with --status")
		statusOnly := fs.Bool("status", false, "Show the current or last upgrade")
		dismiss := fs.Bool("dismiss-warning", false, "Clear the warning that a staged upgrade didn't take effect")
		var imageRef string
		rest := args[1:]
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			imageRef, rest = rest[0], rest[1:]
		}
		_ = fs.Parse(rest)
		if *dismiss {
			_, err := cfg.request("DELETE", "/api/v1/system/upgrade/warning", nil)
			return err
		}
		if !*statusOnly {
			resp, err := cfg.request("POST", "/api/v1/system/upgrade", map[string]any{"image_ref": imageRef})
			if err != nil {
				return err
			}
			if *noWait || cfg.jsonOutput {
				return printJSON(resp)
			}
		}
		// Follow the job, printing its output as it grows.
		printed := 0
		for {
			resp, err := cfg.request("GET", "/api/v1/system/upgrade", nil)
			if err != nil {
				return err
			}
			job, _ := resp["job"].(map[string]any)
			if job == nil {
				fmt.Println("No upgrade has run since velod started.")
				return nil
			}
			out, _ := job["output"].(string)
			if len(out) > printed {
				fmt.Print(out[printed:], "\n")
				printed = len(out) + 1
			}
			switch job["state"] {
			case "SUCCEEDED":
				if st, ok := job["staged"].(map[string]any); ok {
					fmt.Printf("Staged %v (%v); it takes effect at the next reboot: veloctl system reboot\n", st["image"], st["version"])
				} else {
					fmt.Println("Nothing new to stage: already on the newest image.")
				}
				return nil
			case "FAILED":
				return fmt.Errorf("upgrade failed: %v", job["error"])
			}
			if *statusOnly {
				fmt.Printf("Upgrade %v is %v (started %v by %v)\n", job["id"], job["state"], job["started_at"], job["by"])
				return nil
			}
			time.Sleep(3 * time.Second)
		}

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
		fs := flag.NewFlagSet("system reboot", flag.ExitOnError)
		force := fs.Bool("force", false, "Reboot even though hosts are connected (they lose their storage until it's back)")
		_ = fs.Parse(args[1:])
		resp, err := cfg.request("POST", "/api/v1/system/reboot", map[string]any{"force": *force})
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
		return fmt.Errorf("subcommand required: list | scan | claim | release | set")
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
		fmt.Fprintln(tw, "ID\tNAME\tTYPE\tPCI/DEV\tSIZE(GiB)\tDRIVER\tBOOT\tCLAIMED\tPOOL\tWRITE CACHE\tDURABILITY")
		for _, d := range sliceOfMaps(resp["disks"]) {
			loc := fmt.Sprintf("%v", d["pci_address"])
			if loc == "" || loc == "<nil>" {
				loc = fmt.Sprintf("%v", d["device_path"])
			}
			szGiB := uint64FromAny(d["size_bytes"]) / (1024 * 1024 * 1024)
			cache := strOrEmpty(d["write_cache"])
			if cache == "" {
				cache = "UNKNOWN"
			}
			if d["write_cache_policy"] == "DISABLED" {
				cache += " (kept off)"
			}
			durability := strOrEmpty(d["durability"])
			if d["power_loss_protected"] == true {
				durability += " (PLP declared)"
			}
			if d["is_boot_device"] == true {
				cache, durability = "-", "-" // the OS disk is the operating system's business
			}
			fmt.Fprintf(tw, "%v\t%v\t%v\t%s\t%d\t%v\t%v\t%v\t%v\t%s\t%s\n",
				d["id"], d["name"], d["type"], loc, szGiB, d["driver_state"],
				d["is_boot_device"], d["claimed"], strOrEmpty(d["pool_id"]), cache, durability)
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

	case "set":
		fs := flag.NewFlagSet("disk set", flag.ExitOnError)
		plp := fs.String("power-loss-protected", "", "true|false: declare that the disk protects its write cache against power loss")
		cache := fs.String("write-cache", "", "disabled|leave: turn the disk's volatile write cache off and keep it off, or leave it to the drive (switches it back on)")
		_ = fs.Parse(idLast(args[1:]))
		if fs.NArg() < 1 || (*plp == "" && *cache == "") {
			return fmt.Errorf("usage: veloctl disk set <disk-id> [--power-loss-protected=true|false] [--write-cache=disabled|leave]")
		}
		body := map[string]any{}
		switch *plp {
		case "":
		case "true", "false":
			body["power_loss_protected"] = *plp == "true"
		default:
			return fmt.Errorf("--power-loss-protected must be true or false")
		}
		switch *cache {
		case "":
		case "disabled":
			body["write_cache_policy"] = "DISABLED"
		case "leave":
			body["write_cache_policy"] = ""
		default:
			return fmt.Errorf("--write-cache must be disabled or leave")
		}
		resp, err := cfg.request("PATCH", "/api/v1/disks/"+url.PathEscape(fs.Arg(0)), body)
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
		return fmt.Errorf("subcommand required: list | get | create | set-space | replace | start-degraded | release-disk | scrub | importable | import | delete")
	}
	switch args[0] {
	case "importable", "import":
		return runPoolImport(cfg, args)
	case "list":
		resp, err := cfg.request("GET", "/api/v1/pools", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tRAID\tSIZE\tUSED\tCOMMITTED\tAVAILABLE\tSPACE\tDURABILITY\tHEALTH\tDETAILS")
		for _, p := range sliceOfMaps(resp["pools"]) {
			total, free := uint64FromAny(p["total_bytes"]), uint64FromAny(p["free_bytes"])
			used := "-"
			if total > 0 {
				used = fmt.Sprintf("%.0f%%", 100*float64(total-min(free, total))/float64(total))
			}
			fmt.Fprintf(tw, "%v\t%v\t%v\t%s\t%s\t%s\t%s\t%v\t%v\t%v\t%v\n",
				p["id"], p["name"], p["raid_level"], humanBytes(total), used,
				humanBytes(uint64FromAny(p["committed_bytes"])), humanBytes(uint64FromAny(p["available_bytes"])),
				strOrDash(p["space_state"]), strOrDash(p["durability"]), p["health_status"], strOrEmpty(p["status_message"]))
		}
		return tw.Flush()

	case "create":
		fs := flag.NewFlagSet("pool create", flag.ExitOnError)
		name := fs.String("name", "", "Pool name")
		raid := fs.String("raid", "RAID0", "RAID level: RAID0 | RAID1 (RAID10 is not supported yet)")
		disksCSV := fs.String("disks", "", "Comma-separated disk IDs")
		clusterSz := fs.Uint("chunk-size", 1<<20, "Thin allocation unit in bytes (SPDK: the lvstore cluster, default 1 MiB; the kernel data plane always uses 64 KiB)")
		policy := spacePolicyFlags(fs)
		_ = fs.Parse(args[1:])
		if *name == "" || *disksCSV == "" {
			return fmt.Errorf("--name and --disks are required")
		}
		resp, err := cfg.request("POST", "/api/v1/pools", map[string]any{
			"name":             *name,
			"raid_level":       strings.ToUpper(*raid),
			"disk_ids":         strings.Split(*disksCSV, ","),
			"chunk_size_bytes": *clusterSz,
			"space_policy":     policy(),
		})
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "get":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl pool get <pool-id>")
		}
		resp, err := cfg.request("GET", "/api/v1/pools/"+args[1], nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		p, _ := resp["pool"].(map[string]any)
		fmt.Printf("Pool:    %v (%v)\n", p["name"], p["id"])
		fmt.Printf("RAID:    %v on %v\n", p["raid_level"], p["array_name"])
		fmt.Printf("Health:  %v %v\n", p["health_status"], strOrEmpty(p["status_message"]))
		fmt.Printf("Size:    %s total, %s free\n", humanBytes(uint64FromAny(p["total_bytes"])), humanBytes(uint64FromAny(p["free_bytes"])))
		sp, _ := p["space_policy"].(map[string]any)
		fmt.Printf("Space:   %v; %s committed, %s more accepted\n", strOrDash(p["space_state"]),
			humanBytes(uint64FromAny(p["committed_bytes"])), humanBytes(uint64FromAny(p["available_bytes"])))
		fmt.Printf("Policy:  overcommit up to %vx, reserve %v%%, warning at %v%%, critical at %v%% (unset = default)\n",
			orDefault(sp["max_overcommit"], 1.0), orDefault(sp["reserve_percent"], 5.0), orDefault(sp["warning_percent"], 80.0), orDefault(sp["critical_percent"], 90.0))
		tw := newTabWriter()
		fmt.Fprintln(tw, "\nSLOT\tDISK\tSTATE\tREBUILD")
		for _, m := range sliceOfMaps(p["members"]) {
			rebuild := ""
			if m["state"] == "REBUILDING" {
				rebuild = fmt.Sprintf("%d%%", uint64FromAny(m["rebuild_percent"]))
			}
			fmt.Fprintf(tw, "%v\t%v\t%v\t%v\n", uint64FromAny(m["slot"]), strOrDash(m["disk_id"]), m["state"], rebuild)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
		if det := sliceOfAny(p["detached_disk_ids"]); len(det) > 0 {
			fmt.Printf("\nDetached disks (left the RAID; release with `veloctl pool release-disk %v <disk-id>`):\n", p["id"])
			for _, d := range det {
				fmt.Printf("  %v\n", d)
			}
		}
		return nil

	case "replace":
		fs := flag.NewFlagSet("pool replace", flag.ExitOnError)
		newDisk := fs.String("new-disk", "", "Disk to add (an unclaimed disk is wiped and claimed)")
		oldDisk := fs.String("old-disk", "", "Disk to replace (default: the failed or missing one)")
		allowDegraded := fs.Bool("allow-degraded", false, "Allow replacing a disk that still works (the pool has no redundancy until the rebuild completes)")
		id, err := parseWithID(fs, args[1:])
		if err != nil || *newDisk == "" {
			return fmt.Errorf("usage: veloctl pool replace <pool-id> --new-disk <disk-id> [--old-disk <disk-id> --allow-degraded]")
		}
		body := map[string]any{"new_disk_id": *newDisk, "allow_degraded": *allowDegraded}
		if *oldDisk != "" {
			body["old_disk_id"] = *oldDisk
		}
		resp, err := cfg.request("POST", "/api/v1/pools/"+id+"/replace", body)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		p, _ := resp["pool"].(map[string]any)
		fmt.Printf("Rebuilding onto %s: pool %v is %v. Follow progress with `veloctl pool get %v`.\n", *newDisk, id, p["health_status"], id)
		return nil

	case "set-space":
		fs := flag.NewFlagSet("pool set-space", flag.ExitOnError)
		policy := spacePolicyFlags(fs)
		id, err := parseWithID(fs, args[1:])
		if err != nil {
			return fmt.Errorf("usage: veloctl pool set-space <pool-id> [--max-overcommit X] [--reserve-percent N] [--warning-percent N] [--critical-percent N]")
		}
		resp, err := cfg.request("PATCH", "/api/v1/pools/"+id, map[string]any{"space_policy": policy()})
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		p, _ := resp["pool"].(map[string]any)
		fmt.Printf("Pool %v: %v, %s more accepted\n", p["name"], strOrDash(p["space_state"]), humanBytes(uint64FromAny(p["available_bytes"])))
		return nil

	case "start-degraded":
		fs := flag.NewFlagSet("pool start-degraded", flag.ExitOnError)
		confirm := fs.String("confirm", "", "The pool name, to confirm")
		id, err := parseWithID(fs, args[1:])
		if err != nil {
			return fmt.Errorf("usage: veloctl pool start-degraded <pool-id> [--confirm <pool-name>]")
		}
		if *confirm == "" {
			resp, err := cfg.request("GET", "/api/v1/pools/"+id, nil)
			if err != nil {
				return err
			}
			p, _ := resp["pool"].(map[string]any)
			fmt.Fprintf(os.Stderr, "Pool %v: %v\n", p["name"], strOrEmpty(p["status_message"]))
			fmt.Fprintln(os.Stderr, "Starting it degraded drops the missing disk from the array; if it comes back, Velocor leaves it out until you release it or reuse it as a replacement.")
			line, err := promptLine(fmt.Sprintf("Type the pool name (%v) to confirm: ", p["name"]))
			if err != nil {
				return err
			}
			*confirm = line
		}
		resp, err := cfg.request("POST", "/api/v1/pools/"+id+"/start-degraded", map[string]any{"confirm": *confirm})
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		p, _ := resp["pool"].(map[string]any)
		fmt.Printf("Pool %v started: %v %v\nReplace the missing disk with `veloctl pool replace %v --new-disk <disk-id>`.\n", id, p["health_status"], strOrEmpty(p["status_message"]), id)
		return nil

	case "scrub":
		return runPoolScrub(cfg, args[1:])

	case "release-disk":
		if len(args) < 3 {
			return fmt.Errorf("usage: veloctl pool release-disk <pool-id> <disk-id>")
		}
		resp, err := cfg.request("DELETE", "/api/v1/pools/"+args[1]+"/disks/"+args[2], nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		wiped := "not wiped (wipe it before reusing it elsewhere)"
		if resp["wiped"] == true {
			wiped = "and wiped"
		}
		fmt.Printf("Released disk %s from pool %s %s\n", args[2], args[1], wiped)
		return nil

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
		return fmt.Errorf("subcommand required: list | get | usage | create | resize | qos | protect | delete | deleted | restore | purge")
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
		fmt.Fprintln(tw, "ID\tNAME\tPOOL\tSIZE\tALLOCATED\tTHIN\tATTACHMENT\tBDEV\tHEALTH")
		for _, v := range sliceOfMaps(resp["volumes"]) {
			fmt.Fprintf(tw, "%v\t%v\t%v\t%s\t%s\t%v\t%v\t%v\t%v\n",
				v["id"], v["name"], v["pool_id"], humanBytes(uint64FromAny(v["size_bytes"])), humanBytes(uint64FromAny(v["allocated_bytes"])),
				v["thin_provision"], v["attachment_state"], v["device"], v["health_status"])
		}
		return tw.Flush()

	case "create":
		fs := flag.NewFlagSet("volume create", flag.ExitOnError)
		name := fs.String("name", "", "Volume name")
		poolID := fs.String("pool", "", "Pool ID")
		sizeGiB := fs.Uint64("size-gib", 10, "Volume size in GiB")
		thin := fs.Bool("thin", true, "Thin provision volume")
		protect := fs.Bool("protect", false, "Refuse deletion until an administrator turns protection off")
		_ = fs.Parse(args[1:])
		if *name == "" || *poolID == "" {
			return fmt.Errorf("--name and --pool are required")
		}
		resp, err := cfg.request("POST", "/api/v1/volumes", map[string]any{
			"name":                *name,
			"pool_id":             *poolID,
			"size_bytes":          *sizeGiB * 1024 * 1024 * 1024,
			"thin_provision":      *thin,
			"deletion_protection": *protect,
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
		resp, err := cfg.request("DELETE", "/api/v1/volumes/"+url.PathEscape(args[1]), nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		if v, ok := resp["volume"].(map[string]any); ok && v["purge_at"] != nil {
			fmt.Printf("Deleted volume %s; its data is kept until %v ('veloctl volume restore %s' brings it back)\n", args[1], v["purge_at"], args[1])
		} else {
			fmt.Printf("Deleted volume %s\n", args[1])
		}
		return nil

	case "protect":
		fs := flag.NewFlagSet("volume protect", flag.ExitOnError)
		off := fs.Bool("off", false, "Turn deletion protection off instead of on")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: veloctl volume protect [--off] <volume-id>")
		}
		resp, err := cfg.request("PUT", "/api/v1/volumes/"+url.PathEscape(fs.Arg(0))+"/protection", map[string]any{"deletion_protection": !*off})
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		fmt.Printf("Deletion protection of %s: %v\n", fs.Arg(0), !*off)
		return nil

	case "deleted":
		resp, err := cfg.request("GET", "/api/v1/deleted-volumes", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "ID\tNAME\tPOOL\tSIZE\tALLOCATED\tDELETED\tBY\tPURGE AT")
		for _, v := range sliceOfMaps(resp["volumes"]) {
			fmt.Fprintf(tw, "%v\t%v\t%v\t%s\t%s\t%v\t%v\t%v\n",
				v["id"], v["name"], v["pool_id"], humanBytes(uint64FromAny(v["size_bytes"])), humanBytes(uint64FromAny(v["allocated_bytes"])),
				v["deleted_at"], v["deleted_by"], v["purge_at"])
		}
		return tw.Flush()

	case "restore":
		fs := flag.NewFlagSet("volume restore", flag.ExitOnError)
		name := fs.String("name", "", "Restore under this name (if the old one has been reused)")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 {
			return fmt.Errorf("usage: veloctl volume restore [--name <new-name>] <volume-id>")
		}
		var body map[string]any
		if *name != "" {
			body = map[string]any{"name": *name}
		}
		resp, err := cfg.request("POST", "/api/v1/deleted-volumes/"+url.PathEscape(fs.Arg(0))+"/restore", body)
		if err != nil {
			return err
		}
		return printJSON(resp)

	case "purge":
		fs := flag.NewFlagSet("volume purge", flag.ExitOnError)
		yes := fs.Bool("yes", false, "Confirm: the volume's data is destroyed and can't be recovered")
		_ = fs.Parse(args[1:])
		if fs.NArg() != 1 || !*yes {
			return fmt.Errorf("usage: veloctl volume purge --yes <volume-id> (destroys a deleted volume's data now)")
		}
		if _, err := cfg.request("DELETE", "/api/v1/deleted-volumes/"+url.PathEscape(fs.Arg(0)), nil); err != nil {
			return err
		}
		fmt.Printf("Purged volume %s\n", fs.Arg(0))
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
	case "allocated-blocks":
		if len(args) < 2 {
			return fmt.Errorf("usage: veloctl snapshot allocated-blocks <snapshot-id>")
		}
		resp, err := cfg.request("GET", "/api/v1/snapshots/"+url.PathEscape(args[1])+"/blocks:allocated", nil)
		if err != nil {
			return err
		}
		return printBlocks(cfg, resp, "hold data")

	case "changed-blocks":
		fs := flag.NewFlagSet("snapshot changed-blocks", flag.ExitOnError)
		baseID := fs.String("base", "", "The older snapshot")
		targetID := fs.String("target", "", "The newer snapshot")
		_ = fs.Parse(args[1:])
		if *baseID == "" || *targetID == "" {
			return fmt.Errorf("usage: veloctl snapshot changed-blocks --base <older-snapshot-id> --target <newer-snapshot-id>")
		}
		path := fmt.Sprintf("/api/v1/snapshots/%s/blocks:changed?base_snapshot_id=%s", url.PathEscape(*targetID), url.QueryEscape(*baseID))
		resp, err := cfg.request("GET", path, nil)
		if err != nil {
			return err
		}
		return printBlocks(cfg, resp, "changed")

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
		return fmt.Errorf("subcommand required: list | get | connections | access | enable | disable | create | delete")
	}
	switch args[0] {
	case "enable", "disable":
		return runExportEnable(cfg, args)
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
		portals := fs.String("portals", "", "Comma-separated Portal IDs to serve the export on (replaces the current list)")
		id, err := parseWithID(fs, args[1:])
		if err != nil {
			return fmt.Errorf("usage: veloctl export access <export-id> [--initiator-groups a,b] [--allow-any-host=true|false] [--portals p1,p2]")
		}
		body := map[string]any{}
		fs.Visit(func(f *flag.Flag) {
			switch f.Name {
			case "initiator-groups":
				body["initiator_group_ids"] = splitList(*igs)
			case "allow-any-host":
				body["allow_any_host"] = *allowAny
			case "portals":
				body["portal_ids"] = splitList(*portals)
			}
		})
		if len(body) == 0 {
			return fmt.Errorf("nothing to change: give --initiator-groups, --allow-any-host and/or --portals")
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
	timeout := 30 * time.Second
	if method != http.MethodGet {
		timeout = 30 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, rdr)
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
  system            Appliance status, containers, bootc upgrade/rollback, database backup/restore
  disk              List, scan, claim, release drives; set write cache / power-loss protection
  pool              Create, list, inspect or delete pools; space policy; replace a disk, start a pool degraded; import a pool
  volume            Create, list, get, show usage, resize, set QoS, or delete volumes
  snapshot          Create, list, clone and delete snapshots; changed-block tracking
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

// promptLine reads a line from stdin (echoed).
func promptLine(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("read confirmation: %w", err)
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// promptSecret reads a line from the terminal without echoing it (Linux, macOS, Windows). When
// stdin isn't a terminal (a pipe in a script), it reads the line as is.
func promptSecret(prompt string) (string, error) {
	fd := int(os.Stdin.Fd()) //nolint:gosec // a file descriptor/handle always fits in int
	fmt.Fprint(os.Stderr, prompt)
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", fmt.Errorf("read password: %w", err)
		}
		return string(b), nil
	}
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

// idLast moves a leading positional argument behind the flags, so both
// "veloctl disk set <id> --flag" and "veloctl disk set --flag <id>" parse (Go's flag package
// stops at the first non-flag argument).
func idLast(args []string) []string {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return append(append([]string{}, args[1:]...), args[0])
	}
	return args
}

// spacePolicyFlags registers the pool space policy flags; the returned func gives the policy
// with only the flags that were set (unset ones keep the server's defaults).
func spacePolicyFlags(fs *flag.FlagSet) func() map[string]any {
	overcommit := fs.Float64("max-overcommit", 0, "Committed space (volumes fully written + snapshots) may reach this many times the usable capacity (default 1.0: everything fits)")
	reserve := fs.Float64("reserve-percent", -1, "Percent of capacity never committed (default 5)")
	warning := fs.Float64("warning-percent", 0, "Used percent at which the pool warns (default 80)")
	critical := fs.Float64("critical-percent", 0, "Used percent at which all growth is refused (default 90)")
	return func() map[string]any {
		p := map[string]any{}
		if *overcommit > 0 {
			p["max_overcommit"] = *overcommit
		}
		if *reserve >= 0 {
			p["reserve_percent"] = *reserve
		}
		if *warning > 0 {
			p["warning_percent"] = *warning
		}
		if *critical > 0 {
			p["critical_percent"] = *critical
		}
		return p
	}
}

func orDefault(v any, def float64) any {
	if v == nil {
		return def
	}
	return v
}

// runPoolScrub: veloctl pool scrub <pool-id> [--start [--check-only] [--max-mb-per-sec N] | --stop]
// (docs/review-history.md P2-2). Without --start or --stop it shows the pool's current or last scrub.
func runPoolScrub(cfg *cliConfig, args []string) error {
	fs := flag.NewFlagSet("pool scrub", flag.ExitOnError)
	start := fs.Bool("start", false, "Start a scrub now (outside the schedule's window)")
	stop := fs.Bool("stop", false, "Stop the running scrub; the schedule starts the next one afresh")
	checkOnly := fs.Bool("check-only", false, "With --start: count differing mirror copies without rewriting them (unreadable copies are always rewritten)")
	maxMBps := fs.Int("max-mb-per-sec", -1, "With --start: read bandwidth limit (0 = unlimited; default: the schedule's)")
	id, err := parseWithID(fs, args)
	if err != nil || (*start && *stop) {
		return fmt.Errorf("usage: veloctl pool scrub <pool-id> [--start [--check-only] [--max-mb-per-sec N] | --stop]")
	}
	var resp map[string]any
	switch {
	case *start:
		body := map[string]any{"repair": !*checkOnly}
		if *maxMBps >= 0 {
			body["max_mb_per_sec"] = *maxMBps
		}
		resp, err = cfg.request("POST", "/api/v1/pools/"+id+"/scrub", body)
	case *stop:
		resp, err = cfg.request("DELETE", "/api/v1/pools/"+id+"/scrub", nil)
	default:
		resp, err = cfg.request("GET", "/api/v1/pools/"+id+"/scrub", nil)
	}
	if err != nil {
		return err
	}
	if cfg.jsonOutput {
		return printJSON(resp)
	}
	if *stop {
		fmt.Printf("Stopping the scrub of pool %s (it ends after its current window).\n", id)
		return nil
	}
	printScrub(resp)
	return nil
}

func printScrub(resp map[string]any) {
	if why, ok := resp["unavailable"].(string); ok {
		fmt.Printf("Note: %s\n", why)
	}
	sc, _ := resp["scrub"].(map[string]any)
	if sc == nil {
		fmt.Println("Never scrubbed.")
	} else {
		kind := "scheduled"
		if sc["manual"] == true {
			kind = "operator-started"
		}
		pct, _ := sc["percent"].(float64)
		mode := "compare and repair"
		if sc["repair"] != true {
			mode = "check only"
		}
		fmt.Printf("Scrub: %v (%s, %s), %.1f%% done, started %v\n", sc["state"], kind, mode, pct, sc["started_at"])
		if note := strOrEmpty(sc["note"]); note != "" {
			fmt.Printf("  %s\n", note)
		}
		if t, _ := sc["total"].(map[string]any); t != nil {
			fmt.Printf("  Found so far: %d repaired (unreadable copies rewritten), %d differing (%d rewritten), %d unrecoverable, %d failed repair writes (blocks)\n",
				uint64FromAny(t["repaired_blocks"]), uint64FromAny(t["mismatched_blocks"]), uint64FromAny(t["mismatch_repaired_blocks"]),
				uint64FromAny(t["unrecoverable_blocks"]), uint64FromAny(t["write_errors"]))
		}
		if last := strOrEmpty(sc["last_completed_at"]); last != "" {
			fmt.Printf("  Last complete pass: %s\n", last)
		}
	}
	if due, ok := resp["next_due"].(string); ok {
		fmt.Printf("Next scheduled scrub due: %s (it runs in the scrub window)\n", due)
	}
}

// runSystemScrub: veloctl system scrub [--enable | --disable] [--interval-days N] [--window H-H]
// [--max-mb-per-sec N] shows or changes the pools' scrub schedule.
func runSystemScrub(cfg *cliConfig, args []string) error {
	fs := flag.NewFlagSet("system scrub", flag.ExitOnError)
	enable := fs.Bool("enable", false, "Turn scheduled scrubs on")
	disable := fs.Bool("disable", false, "Turn scheduled scrubs off (scrubs can still be started by hand)")
	interval := fs.Int("interval-days", 0, "Scrub each pool every N days")
	window := fs.String("window", "", "Hours (appliance local time) scheduled scrubs run in, e.g. 1-6; equal hours (0-0) mean any time")
	maxMBps := fs.Int("max-mb-per-sec", -1, "Read bandwidth limit of scheduled scrubs, per pool (0 = unlimited)")
	_ = fs.Parse(args)
	if *enable && *disable {
		return fmt.Errorf("--enable and --disable exclude each other")
	}
	body := map[string]any{}
	if *enable || *disable {
		body["disabled"] = *disable
	}
	if *interval > 0 {
		body["interval_days"] = *interval
	}
	if *maxMBps >= 0 {
		body["max_mb_per_sec"] = *maxMBps
	}
	if *window != "" {
		var a, b int
		if _, err := fmt.Sscanf(*window, "%d-%d", &a, &b); err != nil {
			return fmt.Errorf("--window: want START-END hours, e.g. 1-6")
		}
		body["window_start_hour"], body["window_end_hour"] = a, b
	}
	var resp map[string]any
	var err error
	if len(body) == 0 {
		resp, err = cfg.request("GET", "/api/v1/system/scrub", nil)
	} else {
		resp, err = cfg.request("PUT", "/api/v1/system/scrub", body)
	}
	if err != nil {
		return err
	}
	if cfg.jsonOutput {
		return printJSON(resp)
	}
	sc, _ := resp["scrub"].(map[string]any)
	if sc["disabled"] == true {
		fmt.Println("Scheduled scrubs: off")
		return nil
	}
	speed := "unlimited"
	if v := uint64FromAny(sc["max_mb_per_sec"]); v > 0 {
		speed = fmt.Sprintf("at most %d MB/s per pool", v)
	}
	when := fmt.Sprintf("%02d:00-%02d:00", uint64FromAny(sc["window_start_hour"]), uint64FromAny(sc["window_end_hour"]))
	if sc["window_start_hour"] == sc["window_end_hour"] {
		when = "any time"
	}
	fmt.Printf("Scheduled scrubs: every %d days, %s (appliance time), %s\n", uint64FromAny(sc["interval_days"]), when, speed)
	return nil
}

// printBlocks prints a changed-block-tracking answer (docs/review-history.md L-2): the byte ranges and their
// total.
func printBlocks(cfg *cliConfig, resp map[string]any, what string) error {
	if cfg.jsonOutput {
		return printJSON(resp)
	}
	ext, _ := resp["extents"].([]any)
	var total uint64
	tw := newTabWriter()
	fmt.Fprintln(tw, "OFFSET\tLENGTH")
	for _, e := range ext {
		m, _ := e.(map[string]any)
		n := uint64FromAny(m["length_bytes"])
		total += n
		fmt.Fprintf(tw, "%d\t%d\n", uint64FromAny(m["offset_bytes"]), n)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Printf("%d ranges, %d bytes %s, of a %d-byte volume (%d-byte clusters)\n", len(ext), total, what,
		uint64FromAny(resp["volume_size_bytes"]), uint64FromAny(resp["chunk_size_bytes"]))
	return nil
}
