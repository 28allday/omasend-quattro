// Package config loads and persists user settings under the XDG config dir.
package config

import (
	"crypto/tls"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"

	"omasend/internal/protocol"
	"omasend/internal/security"
)

// ExpandHome resolves a leading "~" or "~/" to the user's home directory, so a
// receiveDir stored as "~/Omarchy-Send" (hand-edited, or typed into the
// Settings tab) means what the user means — and is not treated as a relative
// path that silently creates a literal "~" directory under the process cwd.
func ExpandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// Config is the persisted user configuration.
type Config struct {
	Alias       string `json:"alias"`
	Fingerprint string `json:"fingerprint"`
	Port        int    `json:"port"`
	ReceiveDir  string `json:"receiveDir"`
	DeviceModel string `json:"deviceModel"`
	DeviceType  string `json:"deviceType"`
	Protocol    string `json:"protocol"`
	AutoAccept  bool   `json:"autoAccept"`
	PIN         string `json:"pin"`      // if set, senders must supply it
	NoIcons     bool   `json:"noIcons"`  // hide Nerd Font device icons (non-NF terminals)
	NoNotify    bool   `json:"noNotify"` // don't raise desktop notifications on incoming messages/files

	// KnownPeers are hosts (name, IP, or host:port) probed directly over unicast
	// so peers off the local subnet — e.g. reached over Tailscale — show up even
	// though multicast discovery can't find them.
	KnownPeers []string `json:"knownPeers,omitempty"`

	// TLS identity for encrypted (HTTPS) mode, generated once and persisted.
	CertPEM string `json:"certPem"`
	KeyPEM  string `json:"keyPem"`

	// path is where this config was loaded from / will be saved to.
	path string `json:"-"`
}

// Dir returns the config directory, e.g. ~/.config/omasend.
func Dir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "omasend"), nil
}

// legacyConfigPath returns the config.json of a pre-fork omarchy-send install,
// or "" when none exists. Used to seed a first run so the device keeps its
// alias, PIN, receive dir, and TLS identity (peers pin the fingerprint).
func legacyConfigPath() string {
	base, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	p := filepath.Join(base, "omarchy-send", "config.json")
	if _, err := os.Stat(p); err != nil {
		return ""
	}
	return p
}

// defaults returns a Config populated with sensible defaults for this host.
func defaults() Config {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "arch"
	}
	home, _ := os.UserHomeDir()
	return Config{
		// Alias is intentionally empty here; Load generates a random sci-fi
		// alias once on first run (see randomAlias) and persists it. The
		// hostname is still carried in DeviceModel so the machine stays
		// identifiable to peers that look past the display name.
		Alias:       "",
		Port:        protocol.DefaultPort,
		ReceiveDir:  filepath.Join(home, "Omasend"),
		DeviceModel: host,
		DeviceType:  string(protocol.DeviceServer),
		Protocol:    "https",
		AutoAccept:  false,
	}
}

// Load reads the config from dir/config.json, filling defaults for missing
// fields. If the file does not exist it is created. A fingerprint is generated
// and persisted on first run.
func Load() (Config, error) {
	dir, err := Dir()
	if err != nil {
		return Config{}, err
	}
	path := filepath.Join(dir, "config.json")

	cfg := defaults()
	cfg.path = path

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &cfg); err != nil {
			return Config{}, err
		}
		cfg.path = path
	case os.IsNotExist(err):
		// First run: seed from a pre-fork omarchy-send config when present.
		// Load persists to the new path below, so this is a one-time copy.
		// The receive dir is NOT carried over — omasend keeps its own
		// download folder (~/Omasend) so the two apps never mix files.
		if legacy := legacyConfigPath(); legacy != "" {
			if data, lerr := os.ReadFile(legacy); lerr == nil {
				if jerr := json.Unmarshal(data, &cfg); jerr != nil {
					return Config{}, jerr
				}
				cfg.path = path
				cfg.ReceiveDir = defaults().ReceiveDir
			}
		}
	default:
		return Config{}, err
	}

	// Backfill anything still empty after unmarshalling an older/partial file.
	d := defaults()
	// Generate a sci-fi alias once on first run (no file, or a file with no
	// alias). It is persisted below, so the name stays stable across restarts.
	if cfg.Alias == "" {
		cfg.Alias = randomAlias()
	}
	if cfg.Port == 0 {
		cfg.Port = d.Port
	}
	if cfg.ReceiveDir == "" {
		cfg.ReceiveDir = d.ReceiveDir
	}
	// Normalise a ~-form receive dir to absolute; Load persists below, so the
	// stored value is unambiguous from then on.
	cfg.ReceiveDir = ExpandHome(cfg.ReceiveDir)
	if cfg.DeviceType == "" {
		cfg.DeviceType = d.DeviceType
	}
	if cfg.Protocol == "" {
		cfg.Protocol = d.Protocol
	}

	// Generate the TLS identity once. The fingerprint is derived from the
	// certificate (uppercase-hex SHA-256 of its DER), so it is regenerated
	// alongside the cert and persisted.
	if cfg.CertPEM == "" || cfg.KeyPEM == "" {
		id, err := security.Generate()
		if err != nil {
			return Config{}, err
		}
		cfg.CertPEM = id.CertPEM
		cfg.KeyPEM = id.KeyPEM
		cfg.Fingerprint = id.Fingerprint
	}

	if err := cfg.Save(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Save atomically writes the config (temp file + rename).
func (c Config) Save() error {
	if c.path == "" {
		dir, err := Dir()
		if err != nil {
			return err
		}
		c.path = filepath.Join(dir, "config.json")
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// DeviceInfo builds the protocol announcement payload from this config. The
// caller sets Announce as appropriate.
func (c Config) DeviceInfo() protocol.DeviceInfo {
	return protocol.DeviceInfo{
		Alias:       c.Alias,
		Version:     protocol.ProtocolVersion,
		DeviceModel: c.DeviceModel,
		DeviceType:  protocol.DeviceType(c.DeviceType),
		Fingerprint: c.Fingerprint,
		Port:        c.Port,
		Protocol:    c.Protocol,
	}
}

// TLSCertificate returns the parsed TLS keypair for serving HTTPS.
func (c Config) TLSCertificate() (tls.Certificate, error) {
	return tls.X509KeyPair([]byte(c.CertPEM), []byte(c.KeyPEM))
}
