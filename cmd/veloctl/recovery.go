package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Database backups and pool import (TODO P0-3).

// rawRequest sends body as-is and copies a successful response to out.
func (c *cliConfig) rawRequest(method, path string, body io.Reader, out io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint+path, body)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	_, err = io.Copy(out, resp.Body)
	return err
}

// runSystemBackup handles "system backup | backups | restore".
func runSystemBackup(cfg *cliConfig, args []string) error {
	switch args[0] {
	case "backup":
		fs := flag.NewFlagSet("system backup", flag.ExitOnError)
		output := fs.String("o", "", "File to write the database copy to (required; it holds password hashes and secrets: keep it safe)")
		name := fs.String("name", "", "Download this local backup instead of a fresh copy (see `veloctl system backups`)")
		_ = fs.Parse(args[1:])
		if *output == "" {
			return fmt.Errorf("usage: veloctl system backup -o <file> [--name <local backup>]")
		}
		f, err := os.OpenFile(*output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		path := "/api/v1/system/backup"
		if *name != "" {
			path += "?name=" + url.QueryEscape(*name)
		}
		if err := cfg.rawRequest("GET", path, nil, f); err != nil {
			_ = f.Close()
			_ = os.Remove(*output)
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
		fi, _ := os.Stat(*output)
		fmt.Printf("Saved the database (%s) to %s\n", humanBytes(uint64(fi.Size())), *output)
		return nil

	case "backups":
		if len(args) > 1 && args[1] == "create" {
			resp, err := cfg.request("POST", "/api/v1/system/backups", map[string]any{})
			if err != nil {
				return err
			}
			if cfg.jsonOutput {
				return printJSON(resp)
			}
			b, _ := resp["backup"].(map[string]any)
			fmt.Printf("Created backup %v\n", b["name"])
			return nil
		}
		resp, err := cfg.request("GET", "/api/v1/system/backups", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "NAME\tKIND\tCREATED\tSIZE")
		for _, b := range sliceOfMaps(resp["backups"]) {
			fmt.Fprintf(tw, "%v\t%v\t%v\t%s\n", b["name"], b["kind"], b["created_at"], humanBytes(uint64FromAny(b["size_bytes"])))
		}
		return tw.Flush()

	case "restore":
		fs := flag.NewFlagSet("system restore", flag.ExitOnError)
		name := fs.String("name", "", "Restore this local backup (see `veloctl system backups`)")
		yes := fs.Bool("yes", false, "Don't ask for confirmation")
		_ = fs.Parse(args[1:])
		file := fs.Arg(0)
		if (file == "") == (*name == "") {
			return fmt.Errorf("usage: veloctl system restore <file> | --name <local backup> [--yes]")
		}
		if !*yes {
			fmt.Fprintln(os.Stderr, "Restoring replaces the whole configuration (pools, volumes, exports, users, tokens) with the backup's.")
			fmt.Fprintln(os.Stderr, "The current database is backed up first, and velod restarts. Data on the pools is not touched.")
			line, err := promptLine("Type 'restore' to continue: ")
			if err != nil {
				return err
			}
			if strings.TrimSpace(line) != "restore" {
				return fmt.Errorf("cancelled")
			}
		}
		var body io.Reader
		path := "/api/v1/system/restore"
		if *name != "" {
			path += "?name=" + url.QueryEscape(*name)
		} else {
			f, err := os.Open(filepath.Clean(file))
			if err != nil {
				return err
			}
			defer func() { _ = f.Close() }()
			body = f
		}
		if err := cfg.rawRequest("POST", path, body, io.Discard); err != nil {
			return err
		}
		fmt.Println("Restore accepted: velod restarts on the restored database. Sign in again in a few seconds (`veloctl login`).")
		return nil
	}
	return fmt.Errorf("unknown system subcommand %q", args[0])
}

// runPoolImport handles "pool importable | import".
func runPoolImport(cfg *cliConfig, args []string) error {
	switch args[0] {
	case "importable":
		resp, err := cfg.request("GET", "/api/v1/pool-imports", nil)
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		cands := sliceOfMaps(resp["candidates"])
		if len(cands) == 0 {
			fmt.Println("No pools found on unclaimed disks.")
			return nil
		}
		tw := newTabWriter()
		fmt.Fprintln(tw, "POOL ID\tLEVEL\tDISKS\tSIZE\tIMPORTABLE\tNOTE")
		for _, c := range cands {
			var disks []string
			for _, d := range sliceOfMaps(c["disks"]) {
				s := fmt.Sprint(d["disk_id"])
				if d["stale"] == true {
					s += " (left the array)"
				}
				disks = append(disks, s)
			}
			fmt.Fprintf(tw, "%v\t%v\t%v/%v: %s\t%s\t%v\t%s\n", c["pool_id"], c["raid_level"], c["present"], c["members"],
				strings.Join(disks, ", "), humanBytes(uint64FromAny(c["size_bytes"])), c["importable"], strOrEmpty(c["reason"]))
		}
		return tw.Flush()

	case "import":
		fs := flag.NewFlagSet("pool import", flag.ExitOnError)
		name := fs.String("name", "", "Import the pool under this name (when its name is taken here)")
		degraded := fs.Bool("allow-degraded", false, "Start a RAID1 pool that is missing disks without them")
		id, err := parseWithID(fs, args[1:])
		if err != nil {
			return fmt.Errorf("usage: veloctl pool import <pool-id> [--name <name>] [--allow-degraded]")
		}
		resp, err := cfg.request("POST", "/api/v1/pool-imports", map[string]any{"pool_id": id, "name": *name, "allow_degraded": *degraded})
		if err != nil {
			return err
		}
		if cfg.jsonOutput {
			return printJSON(resp)
		}
		p, _ := resp["pool"].(map[string]any)
		from := "the copy on the pool"
		if resp["source"] == "skeleton" {
			from = "SPDK only (no readable copy on the pool)"
		} else if g := uint64FromAny(resp["generation"]); g > 0 {
			from += fmt.Sprintf(" (generation %d, written %v)", g, resp["written_at"])
		}
		fmt.Printf("Imported pool %v (%v) from %s: %v volumes, %v snapshots, %v exports served, %v disabled\n",
			p["name"], p["id"], from, resp["volumes"], resp["snapshots"], resp["exports_enabled"], resp["exports_disabled"])
		for _, w := range sliceOfAny(resp["warnings"]) {
			fmt.Printf("WARNING: %v\n", w)
		}
		return nil
	}
	return fmt.Errorf("unknown pool subcommand %q", args[0])
}

// runExportEnable handles "export enable | disable".
func runExportEnable(cfg *cliConfig, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: veloctl export %s <export-id>", args[0])
	}
	resp, err := cfg.request("PATCH", "/api/v1/exports/"+url.PathEscape(args[1]), map[string]any{"enabled": args[0] == "enable"})
	if err != nil {
		return err
	}
	if cfg.jsonOutput {
		return printJSON(resp)
	}
	fmt.Printf("Export %s %sd\n", args[1], args[0])
	return nil
}
