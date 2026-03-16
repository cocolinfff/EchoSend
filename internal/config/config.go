// Package config handles loading, validating and persisting the EchoSend daemon configuration.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config holds every tunable parameter for the EchoSend daemon.
// All fields map directly to config.yaml keys via yaml struct tags.
type Config struct {
	// NodeName is the human-readable label shown to peers in the LAN.
	NodeName string `yaml:"node_name"`

	// NodeID is a persistent, globally-unique identifier for this node.
	// If left empty in config.yaml it is auto-generated on first run and
	// written back to the file so it survives restarts.
	NodeID string `yaml:"node_id"`

	// DaemonPortUDP is the port on which the UDP gossip listener binds.
	// Used for PRESENCE heartbeats, MSG and FILE_META broadcasts.
	DaemonPortUDP int `yaml:"daemon_port_udp"`

	// DaemonPortTCP is the port on which the TCP file-server listens.
	// Peers connect here to pull files via streaming transfer.
	DaemonPortTCP int `yaml:"daemon_port_tcp"`

	// DaemonPortIPC is the port of the localhost-only HTTP IPC server.
	// Only binds to 127.0.0.1 so CLI sub-processes can talk to the daemon
	// without exposing an extra network surface.
	DaemonPortIPC int `yaml:"daemon_port_ipc"`

	// AutoDownloadMaxMB is the file-size ceiling (in mebibytes) below which
	// the daemon automatically pulls a newly announced file.
	// Set to 0 to disable automatic downloading entirely.
	AutoDownloadMaxMB int `yaml:"auto_download_max_mb"`

	// MaxConcurrentSyncs caps the number of simultaneous file-download
	// goroutines, protecting low-end devices from I/O saturation.
	MaxConcurrentSyncs int `yaml:"max_concurrent_syncs"`

	// StorageDir is the root directory used for the BoltDB database file
	// and all downloaded/seeded files.
	StorageDir string `yaml:"storage_dir"`

	// StaticPeers is an optional list of explicit peer addresses to probe
	// in addition to LAN broadcast discovery.  Each entry may be either a
	// single IPv4 address ("192.168.2.100") or a CIDR block ("10.0.0.0/24").
	StaticPeers []string `yaml:"static_peers"`

	// HeartbeatIntervalSec controls how often (in seconds) the node emits
	// PRESENCE packets to the broadcast address and all static peers.
	HeartbeatIntervalSec int `yaml:"heartbeat_interval_sec"`

	// SeenPacketTTLSec is how long (in seconds) a seen PacketID is kept in
	// the deduplication cache before being evicted.
	SeenPacketTTLSec int `yaml:"seen_packet_ttl_sec"`

	// ProbeRatePerSec limits the number of unicast UDP PRESENCE packets
	// sent per second when enumerating large static-peer CIDR ranges such
	// as /16.  This prevents flooding the system UDP send buffer.
	ProbeRatePerSec int `yaml:"probe_rate_per_sec"`

	// configPath stores the file path from which this config was loaded so
	// that SaveNodeID can write the generated ID back to the same file.
	configPath string `yaml:"-"`
}

// defaults returns a Config pre-populated with sane out-of-the-box values.
func defaults() *Config {
	hostname, _ := os.Hostname()
	if hostname == "" {
		hostname = "node"
	}
	return &Config{
		NodeName:             hostname,
		NodeID:               "",
		DaemonPortUDP:        7777,
		DaemonPortTCP:        7778,
		DaemonPortIPC:        7779,
		AutoDownloadMaxMB:    100,
		MaxConcurrentSyncs:   3,
		StorageDir:           defaultStorageDir(),
		StaticPeers:          []string{},
		HeartbeatIntervalSec: 5,
		SeenPacketTTLSec:     300,
		ProbeRatePerSec:      1000,
	}
}

// defaultStorageDir returns a platform-appropriate default storage path.
func defaultStorageDir() string {
	if runtime.GOOS == "windows" {
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "EchoSend", "data")
		}
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".echosend", "data")
	}
	return "./data"
}

// Load reads config from the YAML file at path, applies defaults for any
// missing fields, generates a NodeID if absent, and returns the result.
// If the file does not exist, a default config is returned and a new file
// is written to path so the user can inspect and customise it.
func Load(path string) (*Config, error) {
	cfg := defaults()
	cfg.configPath = path

	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// First run: persist the defaults so the user can edit them.
			if wErr := cfg.write(path); wErr != nil {
				// Non-fatal: continue with in-memory defaults.
				fmt.Fprintf(os.Stderr, "[config] warning: could not write default config to %s: %v\n", path, wErr)
			}
		} else {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
	} else {
		// Unmarshal into cfg (defaults already set above, so missing yaml keys
		// keep their default values).
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	// Apply per-field defaults for zero values that slipped through.
	applyFallbacks(cfg)

	// Ensure the NodeID is populated; persist it if we just generated one.
	if cfg.NodeID == "" {
		cfg.NodeID = generateNodeID()
		if err := cfg.write(path); err != nil {
			fmt.Fprintf(os.Stderr, "[config] warning: could not persist node_id to %s: %v\n", path, err)
		}
	}

	// Expand StorageDir relative paths to absolute so callers never have to
	// worry about the working directory.
	if !filepath.IsAbs(cfg.StorageDir) {
		base := filepath.Dir(path)
		cfg.StorageDir = filepath.Join(base, cfg.StorageDir)
	}

	if err := cfg.Validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

// AddStaticPeer appends target to StaticPeers (if not already present) and
// persists the updated config back to disk.
func (c *Config) AddStaticPeer(target string) error {
	target = strings.TrimSpace(target)
	for _, existing := range c.StaticPeers {
		if existing == target {
			return nil // already present, nothing to do
		}
	}
	c.StaticPeers = append(c.StaticPeers, target)
	return c.write(c.configPath)
}

// ConfigPath returns the filesystem path from which this config was loaded.
func (c *Config) ConfigPath() string {
	return c.configPath
}

// Save persists the current in-memory config state back to the file it was
// loaded from (or to the path supplied to Load).  Call this after applying
// CLI flag overrides so the new values are durable across restarts.
//
// If configPath is empty (e.g. the struct was constructed manually without
// going through Load) Save returns nil without writing anything.
func (c *Config) Save() error {
	if c.configPath == "" {
		return nil
	}
	return c.write(c.configPath)
}

// AutoDownloadMaxBytes converts AutoDownloadMaxMB to bytes for comparisons.
func (c *Config) AutoDownloadMaxBytes() int64 {
	return int64(c.AutoDownloadMaxMB) * 1024 * 1024
}

// ─────────────────────────────────────────────────────────────────────────────
// Internal helpers
// ─────────────────────────────────────────────────────────────────────────────

// applyFallbacks ensures zero-value fields receive sane defaults after YAML
// unmarshalling, guarding against empty or malformed config files.
func applyFallbacks(c *Config) {
	d := defaults()
	if c.NodeName == "" {
		c.NodeName = d.NodeName
	}
	if c.DaemonPortUDP == 0 {
		c.DaemonPortUDP = d.DaemonPortUDP
	}
	if c.DaemonPortTCP == 0 {
		c.DaemonPortTCP = d.DaemonPortTCP
	}
	if c.DaemonPortIPC == 0 {
		c.DaemonPortIPC = d.DaemonPortIPC
	}
	if c.AutoDownloadMaxMB < 0 {
		c.AutoDownloadMaxMB = d.AutoDownloadMaxMB
	}
	if c.MaxConcurrentSyncs <= 0 {
		c.MaxConcurrentSyncs = d.MaxConcurrentSyncs
	}
	if c.StorageDir == "" {
		c.StorageDir = d.StorageDir
	}
	if c.HeartbeatIntervalSec <= 0 {
		c.HeartbeatIntervalSec = d.HeartbeatIntervalSec
	}
	if c.SeenPacketTTLSec <= 0 {
		c.SeenPacketTTLSec = d.SeenPacketTTLSec
	}
	if c.ProbeRatePerSec <= 0 {
		c.ProbeRatePerSec = d.ProbeRatePerSec
	}
}

// Validate performs semantic checks on the loaded config.
// It is exported so that callers (e.g. the CLI daemon command) can re-run
// validation after applying flag overrides on top of a loaded config.
func (c *Config) Validate() error {
	if c.DaemonPortUDP == c.DaemonPortTCP ||
		c.DaemonPortUDP == c.DaemonPortIPC ||
		c.DaemonPortTCP == c.DaemonPortIPC {
		return fmt.Errorf("config: daemon_port_udp (%d), daemon_port_tcp (%d), and daemon_port_ipc (%d) must all be different",
			c.DaemonPortUDP, c.DaemonPortTCP, c.DaemonPortIPC)
	}
	if c.DaemonPortUDP < 1 || c.DaemonPortUDP > 65535 {
		return fmt.Errorf("config: daemon_port_udp %d is out of range [1, 65535]", c.DaemonPortUDP)
	}
	if c.DaemonPortTCP < 1 || c.DaemonPortTCP > 65535 {
		return fmt.Errorf("config: daemon_port_tcp %d is out of range [1, 65535]", c.DaemonPortTCP)
	}
	if c.DaemonPortIPC < 1 || c.DaemonPortIPC > 65535 {
		return fmt.Errorf("config: daemon_port_ipc %d is out of range [1, 65535]", c.DaemonPortIPC)
	}
	return nil
}

// write marshals the current config to YAML and atomically writes it to path.
func (c *Config) write(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	// Write to a temp file first, then rename for atomicity.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// generateNodeID creates a 16-byte (32-char hex) cryptographically random ID.
func generateNodeID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// Fallback: use hostname + pid (not ideal, but never fails).
		hostname, _ := os.Hostname()
		return fmt.Sprintf("%s-%d", hostname, os.Getpid())
	}
	return hex.EncodeToString(b)
}
