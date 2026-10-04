// SPDX-License-Identifier: AGPL-3.0-only
// Package config loads and validates the YAML configuration once, at startup.
package config

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the parsed file. Secrets are still sources here. It prints as a
// placeholder in logs.
type Config struct {
	SMB        SMBConfig     `yaml:"smb"`
	Storage    StorageConfig `yaml:"storage"`
	S3         S3Config      `yaml:"s3"`
	Logging    LoggingConfig `yaml:"logging"`
	path       string
	Encryption EncryptionConfig `yaml:"encryption"`
	Backup     BackupConfig     `yaml:"backup"`
}

// SMBConfig is the smb section: the listener, the share and its account.
type SMBConfig struct {
	Listen     string `yaml:"listen"`
	Share      string `yaml:"share"`
	Username   string `yaml:"username"`
	Password   string `yaml:"password"`
	ReadOnly   bool   `yaml:"read_only"`
	Encryption bool   `yaml:"encryption"`
}

// StorageConfig is the storage section: local directories and sizes.
type StorageConfig struct {
	CacheSize *ByteSize `yaml:"cache_size"`
	StateDir  string    `yaml:"state_dir"`
	CacheDir  string    `yaml:"cache_dir"`
	Capacity  ByteSize  `yaml:"capacity"`
}

// S3Config is the s3 section: the bucket, the endpoint and its credentials.
type S3Config struct {
	Bucket       string       `yaml:"bucket"`
	Region       string       `yaml:"region"`
	Endpoint     string       `yaml:"endpoint"`
	PathStyle    *bool        `yaml:"path_style"`
	SessionToken string       `yaml:"session_token"`
	TLS          TLSConfig    `yaml:"tls"`
	AccessKey    SecretSource `yaml:"access_key"`
	SecretKey    SecretSource `yaml:"secret_key"`
}

// TLSConfig holds the optional CA and client certificate files for S3.
type TLSConfig struct {
	CAFile         string `yaml:"ca_file"`
	ClientCertFile string `yaml:"client_cert_file"`
	ClientKeyFile  string `yaml:"client_key_file"`
}

// EncryptionConfig is the encryption section for data and metadata in S3.
type EncryptionConfig struct {
	Passphrase SecretSource `yaml:"passphrase"`
	Enabled    bool         `yaml:"enabled"`
}

// BackupConfig is the backup section for metadata backups and trash.
type BackupConfig struct {
	Interval  time.Duration `yaml:"interval"`
	TrashDays int           `yaml:"trash_days"`
}

// LoggingConfig is the logging section.
type LoggingConfig struct {
	Format string `yaml:"format"`
	Level  string `yaml:"level"`
}

// DefaultPath follows XDG on Linux and macOS.
func DefaultPath() (string, error) {
	dir, err := xdg("XDG_CONFIG_HOME", ".config")
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "s3-smb", "config.yaml"), nil
}

func xdg(env, fallback string) (string, error) {
	if dir := os.Getenv(env); dir != "" {
		if !filepath.IsAbs(dir) {
			return "", errors.New(env + " must be absolute")
		}
		return dir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil || !filepath.IsAbs(home) {
		return "", errors.New("an absolute HOME or XDG directory is required")
	}
	return filepath.Join(home, fallback), nil
}

const maxConfigBytes = 1 << 20

// Load reads and validates the YAML file. Resolve reads the secrets and TLS
// files later. Relative paths are resolved against the file's directory.
func Load(path string) (*Config, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, errors.New("resolve configuration path failed")
	}
	f, err := os.OpenFile(filepath.Clean(absolute), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, errors.New("open configuration file failed")
	}
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.Join(errors.New("configuration must be a readable regular file"), f.Close())
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	if err = errors.Join(err, f.Close()); err != nil {
		return nil, errors.New("read configuration file failed")
	}
	if len(data) > maxConfigBytes {
		return nil, errors.New("configuration exceeds 1048576 bytes")
	}
	return parse(data, absolute)
}

func parse(data []byte, path string) (*Config, error) {
	// Check the nodes first. Null, aliases and merge keys hide whether a field
	// was set. YAML error text may quote secrets, so the errors here are fixed.
	var node yaml.Node
	dec := yaml.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&node); err != nil {
		return nil, errors.New("invalid configuration YAML")
	}
	if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode || !plainYAML(&node) {
		return nil, errors.New("configuration must be a mapping without null, aliases or merge keys")
	}
	if err := dec.Decode(new(yaml.Node)); !errors.Is(err, io.EOF) {
		return nil, errors.New("configuration must contain exactly one YAML document")
	}
	dataDir, err := xdg("XDG_DATA_HOME", ".local/share")
	if err != nil {
		return nil, err
	}
	cacheDir, err := xdg("XDG_CACHE_HOME", ".cache")
	if err != nil {
		return nil, err
	}
	c := &Config{
		SMB:        SMBConfig{Listen: "127.0.0.1:445", Share: "TimeMachine", Encryption: true},
		Storage:    StorageConfig{StateDir: filepath.Join(dataDir, "s3-smb"), CacheDir: filepath.Join(cacheDir, "s3-smb")},
		Encryption: EncryptionConfig{Enabled: true}, Backup: BackupConfig{Interval: time.Hour, TrashDays: 14},
		Logging: LoggingConfig{Format: "text", Level: "info"}, path: path,
	}
	dec = yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(c); err != nil {
		return nil, errors.New("invalid configuration YAML: unknown, duplicate or incorrectly typed field")
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	absolute := func(p *string) {
		if *p != "" && !filepath.IsAbs(*p) {
			*p = filepath.Join(dir, *p)
		}
	}
	for _, p := range []*string{&c.Storage.StateDir, &c.Storage.CacheDir, &c.S3.TLS.CAFile, &c.S3.TLS.ClientCertFile, &c.S3.TLS.ClientKeyFile} {
		absolute(p)
	}
	if err := validateCacheDirectory(c.Storage.CacheDir); err != nil {
		return nil, err
	}
	for _, s := range []*SecretSource{&c.S3.AccessKey, &c.S3.SecretKey, &c.Encryption.Passphrase} {
		if s.File != nil {
			absolute(s.File)
		}
		if len(s.Command) > 0 && strings.ContainsRune(s.Command[0], filepath.Separator) {
			absolute(&s.Command[0])
		}
	}
	return c, nil
}

func plainYAML(n *yaml.Node) bool {
	if n.Kind == yaml.AliasNode || n.Tag == "!!null" || n.Tag == "!!merge" {
		return false
	}
	for _, child := range n.Content {
		if !plainYAML(child) {
			return false
		}
	}
	return true
}

func validateCacheDirectory(path string) error {
	if strings.ContainsAny(path, `:,*?[\`) {
		return errors.New("storage.cache_dir must be one directory without path lists, glob characters or backslashes")
	}
	return nil
}

func (c *Config) validate() error {
	if err := c.SMB.validate(); err != nil {
		return err
	}
	if err := c.Storage.validate(); err != nil {
		return err
	}
	if err := c.S3.validate(); err != nil {
		return err
	}
	if c.Encryption.Enabled {
		if err := c.Encryption.Passphrase.validate("encryption.passphrase"); err != nil {
			return err
		}
	}
	if c.Backup.Interval <= 0 {
		return errors.New("backup.interval must be a positive duration")
	}
	if c.Backup.TrashDays < 0 {
		return errors.New("backup.trash_days must be nonnegative")
	}
	if c.Logging.Format != "text" && c.Logging.Format != "json" {
		return errors.New("logging.format must be text or json")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return errors.New("logging.level must be debug, info, warn or error")
	}
	return nil
}

func (c SMBConfig) validate() error {
	_, port, err := net.SplitHostPort(c.Listen)
	p, e := strconv.Atoi(port)
	if err != nil || e != nil || p < 1 || p > 65535 {
		return errors.New("smb.listen requires a host and port from 1 to 65535")
	}
	if c.Share == "" || strings.ContainsAny(c.Share, "/\\\x00") || strings.EqualFold(c.Share, "IPC$") {
		return errors.New("smb.share must be a nonempty share name")
	}
	if c.Username == "" || strings.ContainsRune(c.Username, 0) {
		return errors.New("smb.username is required")
	}
	if c.Password == "" {
		return errors.New("smb.password must be nonempty")
	}
	if strings.ContainsRune(c.Password, 0) {
		return errors.New("smb.password contains NUL")
	}
	return nil
}

func (c StorageConfig) validate() error {
	if c.StateDir == "" || c.CacheDir == "" || strings.ContainsRune(c.StateDir+c.CacheDir, 0) {
		return errors.New("storage directories must be nonempty paths without NUL")
	}
	return validateCacheDirectory(c.CacheDir)
}

func (c S3Config) validate() error {
	if c.Bucket == "" || strings.ContainsAny(c.Bucket, "/\\\x00") {
		return errors.New("s3.bucket is required and must be a bucket name")
	}
	if c.Endpoint != "" {
		u, err := url.Parse(c.Endpoint)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return errors.New("s3.endpoint requires an http or https origin without credentials, query or path")
		}
		if u.Scheme == "http" && (c.TLS.CAFile != "" || c.TLS.ClientCertFile != "" || c.TLS.ClientKeyFile != "") {
			return errors.New("s3 TLS files require an HTTPS endpoint")
		}
	}
	if (c.TLS.ClientCertFile == "") != (c.TLS.ClientKeyFile == "") {
		return errors.New("s3.tls client certificate and key must be supplied together")
	}
	if err := c.AccessKey.validate("s3.access_key"); err != nil {
		return err
	}
	if err := c.SecretKey.validate("s3.secret_key"); err != nil {
		return err
	}
	if strings.ContainsRune(c.SessionToken, 0) {
		return errors.New("s3.session_token contains NUL")
	}
	return nil
}
