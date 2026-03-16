// main.go – EchoSend CLI entry point
//
// # Quick-start (no config file needed):
//
//	echosend daemon --name mynode --udp-port 7777 --tcp-port 7778 --ipc-port 7779
//
// # Or with a config file:
//
//	echosend daemon --config /path/to/config.yaml
//
// # Send a message / file (daemon must be running):
//
//	echosend --send -m "hello LAN"
//	echosend --send -f ./report.pdf
//
// # Show history / peers:
//
//	echosend --history [--limit N] [--json]
//	echosend status
//
// # Manually pull/retry a file by hash:
//
//	echosend --pull <sha256>
//
// # Add a static peer at runtime:
//
//	echosend --add 192.168.2.100
//	echosend --add 10.0.0.0/24
//
// Config resolution for `daemon`:
//  1. Explicit --config flag
//  2. ECHOSEND_CONFIG env-var
//  3. Platform default path  (~/.echosend/config.yaml  /  %APPDATA%\EchoSend\config.yaml)
//  4. ./config.yaml in the working directory
//
// When none of these files exist the daemon starts purely from CLI flags and
// writes a minimal config.yaml to the default platform path so the node-id is
// persisted across restarts.
//
// CLI flag values always override the corresponding config-file values, so you
// can use a shared base config.yaml and customise individual parameters at
// launch time without editing the file.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"p2p-sync/internal/config"
	"p2p-sync/internal/daemon"
	"p2p-sync/internal/ipc"
	"p2p-sync/internal/models"
)

// version is stamped at build time:
//
//	go build -ldflags "-X main.version=$(git describe --tags --always)" .
var version = "dev"

// ─────────────────────────────────────────────────────────────────────────────
// Entry point
// ─────────────────────────────────────────────────────────────────────────────

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	if len(args) == 0 {
		printUsage()
		return 0
	}

	// Fast-path for version / help flags that appear before any sub-command.
	for _, a := range args {
		switch a {
		case "--version", "-version":
			fmt.Printf("echosend %s (%s/%s)\n", version, runtime.GOOS, runtime.GOARCH)
			return 0
		case "--help", "-help", "-h", "help":
			printUsage()
			return 0
		}
	}

	switch args[0] {
	case "daemon":
		return cmdDaemon(args[1:])
	case "status":
		return cmdStatus(args[1:])
	case "--send":
		return cmdSend(args[1:])
	case "--history":
		return cmdHistory(args[1:])
	case "--add":
		return cmdAdd(args[1:])
	case "--pull":
		return cmdPull(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "error: unknown command %q\n\n", args[0])
		printUsage()
		return 1
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// daemon sub-command
// ─────────────────────────────────────────────────────────────────────────────
//
// All config fields are exposed as CLI flags.  When --config points to an
// existing file its values act as defaults; CLI flags always win.
// When no config file is found the daemon boots from flag values alone and
// writes a minimal config.yaml to the resolved path so node-id is stable
// across restarts.

func cmdDaemon(args []string) int {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)

	// ── Flags ─────────────────────────────────────────────────────────────────
	cfgPathFlag  := fs.String("config",   defaultConfigPath(), "path to config.yaml (created if absent)")
	nameFlag     := fs.String("name",     "",                  "node display name shown to peers")
	nodeIDFlag   := fs.String("id",       "",                  "persistent node ID (auto-generated if empty)")
	udpPortFlag  := fs.Int("udp-port",    0,                   "UDP gossip port (default 7777)")
	tcpPortFlag  := fs.Int("tcp-port",    0,                   "TCP file-server port (default 7778)")
	ipcPortFlag  := fs.Int("ipc-port",    0,                   "localhost IPC HTTP port (default 7779)")
	storageFlag  := fs.String("storage",  "",                  "storage directory for DB and files")
	maxDLFlag    := fs.Int("max-dl",      -1,                  "auto-download ceiling in MiB (0 = disabled, default 100)")
	maxSyncFlag  := fs.Int("max-syncs",   0,                   "max concurrent file downloads (default 3)")
	peersFlag    := fs.String("peers",    "",                  "comma-separated static peers: IPs or CIDRs")
	hbFlag       := fs.Int("heartbeat",   0,                   "heartbeat interval in seconds (default 5)")
	probeFlag    := fs.Int("probe-rate",  0,                   "static-peer probe rate packets/sec (default 1000)")

	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: echosend daemon [flags]\n\n")
		fmt.Fprintf(os.Stderr, "Start the EchoSend P2P daemon.  All parameters have sane defaults;\n")
		fmt.Fprintf(os.Stderr, "no config file is required.  A minimal config.yaml is written to\n")
		fmt.Fprintf(os.Stderr, "--config path on first run so the node-id persists across restarts.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	// ── Load (or create) config ───────────────────────────────────────────────
	cfg, err := config.Load(*cfgPathFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: load config: %v\n", err)
		return 1
	}

	// ── Apply CLI overrides (non-zero / non-empty values win) ─────────────────
	if *nameFlag != "" {
		cfg.NodeName = *nameFlag
	}
	if *nodeIDFlag != "" {
		cfg.NodeID = *nodeIDFlag
	}
	if *udpPortFlag != 0 {
		cfg.DaemonPortUDP = *udpPortFlag
	}
	if *tcpPortFlag != 0 {
		cfg.DaemonPortTCP = *tcpPortFlag
	}
	if *ipcPortFlag != 0 {
		cfg.DaemonPortIPC = *ipcPortFlag
	}
	if *storageFlag != "" {
		// Resolve to absolute path now, while the CLI's CWD is known.
		// This prevents the stored path from breaking if the daemon later
		// changes its working directory.
		absStorage, absErr := filepath.Abs(*storageFlag)
		if absErr != nil {
			fmt.Fprintf(os.Stderr, "error: resolve --storage path %q: %v\n", *storageFlag, absErr)
			return 1
		}
		cfg.StorageDir = absStorage
	}
	if *maxDLFlag >= 0 { // -1 means "not set"; 0 means "disable"
		cfg.AutoDownloadMaxMB = *maxDLFlag
	}
	if *maxSyncFlag > 0 {
		cfg.MaxConcurrentSyncs = *maxSyncFlag
	}
	if *hbFlag > 0 {
		cfg.HeartbeatIntervalSec = *hbFlag
	}
	if *probeFlag > 0 {
		cfg.ProbeRatePerSec = *probeFlag
	}
	if *peersFlag != "" {
		for _, p := range strings.Split(*peersFlag, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				if addErr := cfg.AddStaticPeer(p); addErr != nil {
					fmt.Fprintf(os.Stderr, "warning: --peers: could not add %q: %v\n", p, addErr)
				}
			}
		}
	}

	// Validate after overrides (ports must remain distinct, etc.).
	if err := cfg.Validate(); err != nil {
		fmt.Fprintf(os.Stderr, "error: invalid configuration: %v\n", err)
		return 1
	}

	// Persist the final (overridden) config back to disk so that:
	//   • The node-id is durable across restarts even when started flag-only.
	//   • CLI-overridden values (ports, storage path, …) become the new
	//     defaults the next time the daemon starts without explicit flags.
	if saveErr := cfg.Save(); saveErr != nil {
		// Non-fatal: the daemon can run fine without persisting; warn only.
		fmt.Fprintf(os.Stderr, "warning: could not save config to %s: %v\n",
			cfg.ConfigPath(), saveErr)
	}

	// Ensure the storage directory exists before BoltDB tries to open a file.
	if err := os.MkdirAll(cfg.StorageDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: create storage dir %s: %v\n", cfg.StorageDir, err)
		return 1
	}

	// Print a concise startup summary.
	fmt.Printf("EchoSend %s  node=%s  id=%s\n", version, cfg.NodeName, cfg.NodeID[:min(12, len(cfg.NodeID))]+"…")
	fmt.Printf("  UDP :%d   TCP :%d   IPC 127.0.0.1:%d\n", cfg.DaemonPortUDP, cfg.DaemonPortTCP, cfg.DaemonPortIPC)
	fmt.Printf("  storage : %s\n", cfg.StorageDir)
	fmt.Printf("  config  : %s\n\n", cfg.ConfigPath())

	d, err := daemon.New(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: init daemon: %v\n", err)
		return 1
	}

	if err := d.Run(context.Background()); err != nil {
		fmt.Fprintf(os.Stderr, "daemon error: %v\n", err)
		return 1
	}
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// status sub-command
// ─────────────────────────────────────────────────────────────────────────────

func cmdStatus(args []string) int {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	cfgPath  := fs.String("config",   defaultConfigPath(), "path to config.yaml")
	ipcPort  := fs.Int("ipc-port",    0,                   "override IPC port")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: echosend status [flags]\n\nShow daemon node info and known peers.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	port, err := resolveIPCPort(*cfgPath, *ipcPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	client := ipc.NewClient(port)
	if pingErr := client.Ping(); pingErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", pingErr)
		return 1
	}

	st, err := client.Status()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	fmt.Printf("Node  : %s\n", st.NodeName)
	fmt.Printf("ID    : %s\n", st.NodeID)
	fmt.Printf("Peers : %d\n\n", st.PeerCount)

	if len(st.Peers) == 0 {
		fmt.Println("No peers discovered yet.")
		return 0
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tID\tIP\tUDP\tTCP\tLAST SEEN")
	fmt.Fprintln(tw, "────\t──\t──\t───\t───\t─────────")
	for _, p := range st.Peers {
		id := p.NodeID
		if len(id) > 12 {
			id = id[:12] + "…"
		}
		ls := time.Unix(0, p.LastSeen).Local().Format("15:04:05")
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\n",
			p.NodeName, id, p.IP, p.UDPPort, p.TCPPort, ls)
	}
	tw.Flush()
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// --send
// ─────────────────────────────────────────────────────────────────────────────

func cmdSend(args []string) int {
	fs := flag.NewFlagSet("--send", flag.ExitOnError)
	cfgPath  := fs.String("config",   defaultConfigPath(), "path to config.yaml")
	ipcPort  := fs.Int("ipc-port",    0,                   "override IPC port")
	msgFlag  := fs.String("m",        "",                  "text message to broadcast")
	fileFlag := fs.String("f",        "",                  "path to file to broadcast")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  echosend --send -m \"hello world\"\n")
		fmt.Fprintf(os.Stderr, "  echosend --send -f ./report.pdf\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *msgFlag == "" && *fileFlag == "" {
		fmt.Fprintf(os.Stderr, "error: --send requires -m <text> or -f <path>\n\n")
		fs.Usage()
		return 1
	}
	if *msgFlag != "" && *fileFlag != "" {
		fmt.Fprintf(os.Stderr, "error: -m and -f are mutually exclusive\n")
		return 1
	}

	port, err := resolveIPCPort(*cfgPath, *ipcPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	client := ipc.NewClient(port)
	if pingErr := client.Ping(); pingErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", pingErr)
		return 1
	}

	// ── Text message ──────────────────────────────────────────────────────────
	if *msgFlag != "" {
		msg, err := client.SendMessage(*msgFlag)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: send message: %v\n", err)
			return 1
		}
		fmt.Printf("✓ Message broadcast  id=%s\n", msg.MessageID)
		return 0
	}

	// ── File ──────────────────────────────────────────────────────────────────
	absPath, err := filepath.Abs(*fileFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: resolve path %q: %v\n", *fileFlag, err)
		return 1
	}
	if _, statErr := os.Stat(absPath); os.IsNotExist(statErr) {
		fmt.Fprintf(os.Stderr, "error: file not found: %s\n", absPath)
		return 1
	}

	if err := client.SendFile(absPath); err != nil {
		fmt.Fprintf(os.Stderr, "error: send file: %v\n", err)
		return 1
	}

	fmt.Printf("✓ File accepted for broadcast: %s\n", filepath.Base(absPath))
	fmt.Println("  The daemon is hashing and will broadcast FILE_META to all peers.")
	fmt.Println("  Use --history to track progress.")
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// --history
// ─────────────────────────────────────────────────────────────────────────────

func cmdHistory(args []string) int {
	fs := flag.NewFlagSet("--history", flag.ExitOnError)
	cfgPath  := fs.String("config",   defaultConfigPath(), "path to config.yaml")
	ipcPort  := fs.Int("ipc-port",    0,                   "override IPC port")
	limitFlag := fs.Int("limit",      20,                  "max entries to show (0 = all)")
	jsonFlag  := fs.Bool("json",      false,               "output raw JSON")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage: echosend --history [flags]\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	port, err := resolveIPCPort(*cfgPath, *ipcPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	client := ipc.NewClient(port)
	if pingErr := client.Ping(); pingErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", pingErr)
		return 1
	}

	hist, err := client.History(*limitFlag)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: history: %v\n", err)
		return 1
	}

	if *jsonFlag {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(hist)
		return 0
	}

	printHistoryFormatted(hist)
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// --add
// ─────────────────────────────────────────────────────────────────────────────

func cmdAdd(args []string) int {
	fs := flag.NewFlagSet("--add", flag.ExitOnError)
	cfgPath := fs.String("config",   defaultConfigPath(), "path to config.yaml")
	ipcPort := fs.Int("ipc-port",    0,                   "override IPC port")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  echosend --add 192.168.2.100\n")
		fmt.Fprintf(os.Stderr, "  echosend --add 10.0.0.0/24\n\nFlags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() == 0 {
		fmt.Fprintf(os.Stderr, "error: --add requires a target IP or CIDR\n\n")
		fs.Usage()
		return 1
	}

	target := strings.TrimSpace(fs.Arg(0))

	port, err := resolveIPCPort(*cfgPath, *ipcPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	client := ipc.NewClient(port)
	if pingErr := client.Ping(); pingErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", pingErr)
		return 1
	}

	if err := client.AddPeer(target); err != nil {
		fmt.Fprintf(os.Stderr, "error: add peer: %v\n", err)
		return 1
	}

	fmt.Printf("✓ Static peer added: %s\n", target)
	fmt.Println("  Daemon will probe this target on the next heartbeat cycle.")
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// --pull
// ─────────────────────────────────────────────────────────────────────────────

func cmdPull(args []string) int {
	fs := flag.NewFlagSet("--pull", flag.ExitOnError)
	cfgPath := fs.String("config", defaultConfigPath(), "path to config.yaml")
	ipcPort := fs.Int("ipc-port", 0, "override IPC port")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage:\n")
		fmt.Fprintf(os.Stderr, "  echosend --pull <sha256-file-hash>\n\n")
		fmt.Fprintf(os.Stderr, "Trigger a manual pull/retry for a file already present in history metadata.\n\n")
		fmt.Fprintf(os.Stderr, "Flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if fs.NArg() != 1 {
		fmt.Fprintf(os.Stderr, "error: --pull requires exactly one file hash argument\n\n")
		fs.Usage()
		return 1
	}

	fileHash := strings.TrimSpace(fs.Arg(0))
	if fileHash == "" {
		fmt.Fprintf(os.Stderr, "error: file hash must not be empty\n")
		return 1
	}

	port, err := resolveIPCPort(*cfgPath, *ipcPort)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		return 1
	}

	client := ipc.NewClient(port)
	if pingErr := client.Ping(); pingErr != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", pingErr)
		return 1
	}

	if err := client.PullFileByHash(fileHash); err != nil {
		fmt.Fprintf(os.Stderr, "error: pull file: %v\n", err)
		return 1
	}

	fmt.Printf("✓ Manual pull scheduled for hash: %s\n", fileHash)
	fmt.Println("  Use --history to track download status changes.")
	return 0
}

// ─────────────────────────────────────────────────────────────────────────────
// History rendering
// ─────────────────────────────────────────────────────────────────────────────

type historyEntry struct {
	ts   int64
	line string
}

func printHistoryFormatted(hist *models.IPCHistoryResp) {
	var entries []historyEntry

	for _, m := range hist.Messages {
		ts := time.Unix(0, m.Timestamp).Local().Format("2006-01-02 15:04:05")
		name := m.SenderName
		if name == "" && len(m.SenderID) >= 8 {
			name = m.SenderID[:8]
		}
		entries = append(entries, historyEntry{
			ts:   m.Timestamp,
			line: fmt.Sprintf("  [%s] MSG  %-16s %s", ts, name, m.Content),
		})
	}

	for _, f := range hist.Files {
		ts := time.Unix(0, f.Timestamp).Local().Format("2006-01-02 15:04:05")
		hashPfx := f.FileHash
		if len(hashPfx) > 12 {
			hashPfx = hashPfx[:12] + "…"
		}
		entries = append(entries, historyEntry{
			ts: f.Timestamp,
			line: fmt.Sprintf("  [%s] FILE %-24s %s  %s  hash=%s",
				ts, f.FileName, humanSize(f.FileSize), fileStatusBadge(string(f.Status)), hashPfx),
		})
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].ts > entries[j].ts })

	if len(entries) == 0 {
		fmt.Println("No history yet.")
		return
	}

	fmt.Println("─── History (newest first) ───────────────────────────────────")
	for _, e := range entries {
		fmt.Println(e.line)
	}
	fmt.Println("──────────────────────────────────────────────────────────────")
}

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────

// resolveIPCPort returns the IPC port to use.
//   - If cliOverride > 0 it is used directly (no config loading).
//   - Otherwise the config at cfgPath is loaded and its DaemonPortIPC is used.
//   - If cfgPath does not exist, the built-in default port (7779) is returned.
func resolveIPCPort(cfgPath string, cliOverride int) (int, error) {
	if cliOverride > 0 {
		return cliOverride, nil
	}
	// Try loading the config; fall back to default if the file doesn't exist.
	cfg, err := config.Load(cfgPath)
	if err != nil {
		// config.Load already handles missing files by returning defaults,
		// so a real error here means the file exists but is malformed.
		return 0, fmt.Errorf("load config %s: %w", cfgPath, err)
	}
	return cfg.DaemonPortIPC, nil
}

// defaultConfigPath returns the platform-appropriate default config path.
// Resolution order: ECHOSEND_CONFIG env-var → platform user dir → ./config.yaml
func defaultConfigPath() string {
	if ev := os.Getenv("ECHOSEND_CONFIG"); ev != "" {
		return ev
	}
	if runtime.GOOS == "windows" {
		if ad := os.Getenv("APPDATA"); ad != "" {
			return filepath.Join(ad, "EchoSend", "config.yaml")
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".echosend", "config.yaml")
	}
	return "config.yaml"
}

// loadCfgAndPort is a convenience wrapper kept for any code that still uses it.
func loadCfgAndPort(cfgPath string) (*config.Config, int, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, 0, fmt.Errorf("load config %s: %w", cfgPath, err)
	}
	return cfg, cfg.DaemonPortIPC, nil
}

// printUsage prints the top-level help text.
func printUsage() {
	fmt.Printf(`EchoSend %s – LAN P2P sync daemon

USAGE:
  echosend daemon [flags]              Start the P2P daemon (foreground)
  echosend status  [flags]             Show daemon status and peer list

  echosend --send -m "text" [flags]    Broadcast a text message
  echosend --send -f ./file [flags]    Broadcast a file (peers auto-download)

  echosend --history [flags]           Show message + file history
  echosend --add <ip|cidr> [flags]     Add a static peer / CIDR block
	echosend --pull <sha256> [flags]     Manually pull/retry a file download

DAEMON FLAGS (all optional – sane defaults apply):
  --config   path   Config file path; created with defaults if absent
  --name     str    Node display name   (default: hostname)
  --id       str    Node ID             (default: auto-generated, persisted)
  --udp-port int    UDP gossip port     (default: 7777)
  --tcp-port int    TCP file port       (default: 7778)
  --ipc-port int    Localhost IPC port  (default: 7779)
  --storage  path   Data directory      (default: platform user dir)
  --max-dl   int    Auto-DL ceiling MiB (default: 100, 0=disable)
  --max-syncs int   Concurrent DLs      (default: 3)
  --peers    str    Static peers, comma-separated IPs/CIDRs
  --heartbeat int   Heartbeat seconds   (default: 5)
  --probe-rate int  CIDR probe pkt/sec  (default: 1000)

CLIENT FLAGS (--send / --history / --add / status):
  --config   path   Config file to read IPC port from
  --ipc-port int    Direct IPC port override (skip config lookup)

GLOBAL:
  --version         Print version and exit
  --help / -h       Print this help

ENVIRONMENT:
  ECHOSEND_CONFIG   Override config file path

EXAMPLES:
  # Start with no config file:
  echosend daemon --name alice --storage ./alice-data

  # Start on non-default ports:
  echosend daemon --udp-port 8877 --tcp-port 8878 --ipc-port 8879

  # Send a message to the running daemon:
  echosend --send -m "deploying v2 now"

  # Share a file:
  echosend --send -f ./build/app.tar.gz

  # Add a cross-subnet peer:
  echosend --add 10.10.0.0/24

	# Retry a file download by hash:
	echosend --pull 3f8a1c09b2d70b7e2dbd7f9fbd3e2cce8e8b6c63f2a4e3e0fd3af1c8a9d4b2ef

`, version)
}

// fileStatusBadge returns a compact ASCII status label.
func fileStatusBadge(status string) string {
	switch status {
	case "SEEDING":
		return "[seed]"
	case "COMPLETE":
		return "[done]"
	case "DOWNLOADING":
		return "[dl..]"
	case "FAILED":
		return "[fail]"
	default:
		return "[known]"
	}
}

// humanSize formats a byte count as a human-readable string.
func humanSize(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%dB", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(b)/float64(div), "KMGTPE"[exp])
}

// min returns the smaller of two ints. (pre-Go-1.21 compatible)
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
