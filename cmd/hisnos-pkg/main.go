// cmd/hisnos-pkg/main.go — HisnOS package store CLI.
//
// Usage:
//
//	hisnos-pkg list                   List available plugins from catalogue
//	hisnos-pkg installed              List locally installed plugins
//	hisnos-pkg install <name>         Download, verify, and install a plugin
//	hisnos-pkg uninstall <name>       Remove an installed plugin
//	hisnos-pkg enable <name>          Enable a disabled plugin
//	hisnos-pkg disable <name>         Disable (but do not remove) a plugin
//	hisnos-pkg info <name>            Show plugin details from catalogue
//	hisnos-pkg run <name> [args...]   Execute a plugin in its bwrap sandbox
//
// All install operations require root (uid=0).
// Catalogue URL can be overridden with HISNOS_MARKETPLACE_URL env var.
//
// Exit codes:
//
//	0  success
//	1  usage error
//	2  operation failed
//	3  permission denied (needs root)
package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultCatalogueURL = "https://marketplace.hisnos.dev/catalogue.json"
	indexPath           = "/var/lib/hisnos/marketplace-index.json"
	marketplaceBase     = "/var/lib/hisnos/marketplace"
)

// PluginManifest mirrors the marketplace registry struct (no import needed).
type PluginManifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Author      string `json:"author"`
	ArchiveURL  string `json:"archive_url"`
	SHA256      string `json:"sha256"`
	MinVersion  string `json:"min_version"`
	Arch        string `json:"arch"`
	Sandbox     string `json:"sandbox"`
}

// InstalledPlugin mirrors the marketplace registry installed record.
type InstalledPlugin struct {
	Name        string    `json:"name"`
	Version     string    `json:"version"`
	InstallDir  string    `json:"install_dir"`
	InstalledAt time.Time `json:"installed_at"`
	Sandbox     string    `json:"sandbox"`
	Enabled     bool      `json:"enabled"`
}

type marketplaceIndex struct {
	Installed map[string]*InstalledPlugin `json:"installed"`
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "list":
		cmdList()
	case "installed":
		cmdInstalled()
	case "install":
		requireArgs(args, 1, "install <name>")
		requireRoot()
		cmdInstall(args[0])
	case "uninstall":
		requireArgs(args, 1, "uninstall <name>")
		requireRoot()
		cmdUninstall(args[0])
	case "enable":
		requireArgs(args, 1, "enable <name>")
		requireRoot()
		cmdSetEnabled(args[0], true)
	case "disable":
		requireArgs(args, 1, "disable <name>")
		requireRoot()
		cmdSetEnabled(args[0], false)
	case "info":
		requireArgs(args, 1, "info <name>")
		cmdInfo(args[0])
	case "run":
		requireArgs(args, 1, "run <name> [args...]")
		cmdRun(args[0], args[1:])
	case "help", "--help", "-h":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "hisnos-pkg: unknown command %q\n", cmd)
		usage()
		os.Exit(1)
	}
}

// cmdList fetches the remote catalogue and prints available plugins.
func cmdList() {
	manifests, err := fetchCatalogue()
	if err != nil {
		fatalf(2, "fetch catalogue: %v", err)
	}
	idx := loadIndex()
	fmt.Printf("%-24s %-10s %-10s %s\n", "NAME", "VERSION", "SANDBOX", "DESCRIPTION")
	fmt.Println(strings.Repeat("-", 72))
	for _, m := range manifests {
		installed := ""
		if p, ok := idx.Installed[m.Name]; ok {
			installed = " [installed"
			if p.Enabled {
				installed += ",enabled"
			}
			installed += "]"
		}
		fmt.Printf("%-24s %-10s %-10s %s%s\n",
			m.Name, m.Version, m.Sandbox, truncate(m.Description, 32), installed)
	}
}

// cmdInstalled prints locally installed plugins.
func cmdInstalled() {
	idx := loadIndex()
	if len(idx.Installed) == 0 {
		fmt.Println("No plugins installed.")
		return
	}
	fmt.Printf("%-24s %-10s %-10s %-8s %s\n", "NAME", "VERSION", "SANDBOX", "ENABLED", "INSTALLED AT")
	fmt.Println(strings.Repeat("-", 72))
	for _, p := range idx.Installed {
		enabled := "no"
		if p.Enabled {
			enabled = "yes"
		}
		fmt.Printf("%-24s %-10s %-10s %-8s %s\n",
			p.Name, p.Version, p.Sandbox, enabled, p.InstalledAt.Format("2006-01-02"))
	}
}

// cmdInstall calls hisnosd IPC to install a plugin by name.
// Fetches the catalogue to find the manifest, then delegates to the registry
// daemon via IPC socket.
func cmdInstall(name string) {
	manifests, err := fetchCatalogue()
	if err != nil {
		fatalf(2, "fetch catalogue: %v", err)
	}
	var found *PluginManifest
	for i := range manifests {
		if manifests[i].Name == name {
			found = &manifests[i]
			break
		}
	}
	if found == nil {
		fatalf(2, "plugin %q not found in catalogue", name)
	}

	if found.Sandbox == "privileged" {
		fmt.Printf("WARNING: Plugin %q requires privileged sandbox.\n", name)
		fmt.Printf("This grants the plugin direct system access without isolation.\n")
		fmt.Printf("Type 'yes' to confirm: ")
		var confirm string
		fmt.Scanln(&confirm)
		if confirm != "yes" {
			fmt.Println("Aborted.")
			os.Exit(0)
		}
	}

	fmt.Printf("Installing %s@%s (sandbox=%s)...\n", found.Name, found.Version, found.Sandbox)

	result, err := ipcCall("marketplace_install", map[string]any{
		"name":        found.Name,
		"version":     found.Version,
		"archive_url": found.ArchiveURL,
		"sha256":      found.SHA256,
		"signature":   "", // fetched by registry
		"sandbox":     found.Sandbox,
	})
	if err != nil {
		fatalf(2, "install: %v", err)
	}
	if errMsg, ok := result["error"].(string); ok && errMsg != "" {
		fatalf(2, "install: %s", errMsg)
	}
	fmt.Printf("✓ Installed %s@%s\n", found.Name, found.Version)
	fmt.Printf("  Run 'hisnos-pkg enable %s' to activate it.\n", found.Name)
}

// cmdUninstall removes an installed plugin.
func cmdUninstall(name string) {
	result, err := ipcCall("marketplace_uninstall", map[string]any{"name": name})
	if err != nil {
		fatalf(2, "uninstall: %v", err)
	}
	if errMsg, ok := result["error"].(string); ok && errMsg != "" {
		fatalf(2, "uninstall: %s", errMsg)
	}
	fmt.Printf("✓ Uninstalled %s\n", name)
}

// cmdSetEnabled enables or disables a plugin.
func cmdSetEnabled(name string, enabled bool) {
	action := "marketplace_enable"
	verb := "Enabled"
	if !enabled {
		action = "marketplace_disable"
		verb = "Disabled"
	}
	result, err := ipcCall(action, map[string]any{"name": name})
	if err != nil {
		fatalf(2, "%s: %v", strings.ToLower(verb), err)
	}
	if errMsg, ok := result["error"].(string); ok && errMsg != "" {
		fatalf(2, "%s: %s", strings.ToLower(verb), errMsg)
	}
	fmt.Printf("✓ %s %s\n", verb, name)
}

// cmdInfo displays detailed plugin information from the catalogue.
func cmdInfo(name string) {
	manifests, err := fetchCatalogue()
	if err != nil {
		fatalf(2, "fetch catalogue: %v", err)
	}
	for _, m := range manifests {
		if m.Name != name {
			continue
		}
		fmt.Printf("Name:        %s\n", m.Name)
		fmt.Printf("Version:     %s\n", m.Version)
		fmt.Printf("Author:      %s\n", m.Author)
		fmt.Printf("Description: %s\n", m.Description)
		fmt.Printf("Sandbox:     %s\n", m.Sandbox)
		fmt.Printf("Arch:        %s\n", m.Arch)
		if m.MinVersion != "" {
			fmt.Printf("Min HisnOS:  %s\n", m.MinVersion)
		}
		fmt.Printf("SHA-256:     %s\n", m.SHA256)
		return
	}
	fatalf(2, "plugin %q not found in catalogue", name)
}

// cmdRun executes an installed, enabled plugin.
func cmdRun(name string, runArgs []string) {
	result, err := ipcCall("marketplace_run", map[string]any{
		"name": name, "args": runArgs,
	})
	if err != nil {
		fatalf(2, "run: %v", err)
	}
	if errMsg, ok := result["error"].(string); ok && errMsg != "" {
		fatalf(2, "run: %s", errMsg)
	}
}

// ─── helpers ────────────────────────────────────────────────────────────────

func fetchCatalogue() ([]PluginManifest, error) {
	url := os.Getenv("HISNOS_MARKETPLACE_URL")
	if url == "" {
		url = defaultCatalogueURL
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var manifests []PluginManifest
	return manifests, json.NewDecoder(resp.Body).Decode(&manifests)
}

func loadIndex() marketplaceIndex {
	idx := marketplaceIndex{Installed: make(map[string]*InstalledPlugin)}
	data, err := os.ReadFile(indexPath)
	if err != nil {
		return idx
	}
	_ = json.Unmarshal(data, &idx)
	if idx.Installed == nil {
		idx.Installed = make(map[string]*InstalledPlugin)
	}
	return idx
}

// ipcCall sends a JSON-RPC request to hisnosd via its IPC socket.
func ipcCall(method string, params map[string]any) (map[string]any, error) {
	socketPath := ipcSocketPath()
	conn, err := dialUnix(socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to hisnosd (%s): %w", socketPath, err)
	}
	defer conn.Close()

	req := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  method,
		"params":  params,
	}
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return nil, fmt.Errorf("send: %w", err)
	}
	var resp map[string]any
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return nil, fmt.Errorf("recv: %w", err)
	}
	if result, ok := resp["result"].(map[string]any); ok {
		return result, nil
	}
	if errObj, ok := resp["error"].(map[string]any); ok {
		msg, _ := errObj["message"].(string)
		return nil, fmt.Errorf("IPC error: %s", msg)
	}
	return resp, nil
}

func ipcSocketPath() string {
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		return xdg + "/hisnosd.sock"
	}
	return "/run/hisnosd/hisnosd.sock"
}

func requireRoot() {
	if os.Getuid() != 0 {
		fatalf(3, "this operation requires root (uid=0)")
	}
}

func requireArgs(args []string, n int, usage string) {
	if len(args) < n {
		fatalf(1, "usage: hisnos-pkg %s", usage)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-3] + "..."
}

func fatalf(code int, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "hisnos-pkg: "+format+"\n", args...)
	os.Exit(code)
}

// dialUnix opens a Unix socket connection with a 5-second timeout.
func dialUnix(path string) (net.Conn, error) {
	return net.DialTimeout("unix", path, 5*time.Second)
}

func usage() {
	fmt.Print(`Usage: hisnos-pkg <command> [args]

Commands:
  list                  List available plugins from catalogue
  installed             List locally installed plugins
  install <name>        Download, verify, and install a plugin
  uninstall <name>      Remove an installed plugin
  enable <name>         Enable a disabled plugin
  disable <name>        Disable (but keep) a plugin
  info <name>           Show plugin details
  run <name> [args...]  Execute plugin in sandbox

Environment:
  HISNOS_MARKETPLACE_URL  Override catalogue URL

`)
}
