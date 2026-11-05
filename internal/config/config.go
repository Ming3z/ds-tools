// Package config loads and persists the ds-tools configuration file.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Environment variables understood by ds-tools.
const (
	EnvAPIKey  = "DEEPSEEK_API_KEY"
	EnvBaseURL = "DEEPSEEK_BASE_URL"
	EnvConfig  = "DS_TOOLS_CONFIG"
	EnvFormat  = "DS_TOOLS_FORMAT"
	EnvSkill   = "DS_TOOLS_SKILL_DIR"
	EnvNoColor = "NO_COLOR"
)

// Defaults used when neither flags nor the config file provide a value.
const (
	DefaultBaseURL  = "https://api.deepseek.com"
	DefaultTimeout  = 30 * time.Second
	DefaultRetries  = 2
	DefaultCacheTTL = time.Hour
)

// Config mirrors config.json.
type Config struct {
	Version  int    `json:"version"`
	APIKey   string `json:"api_key,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Timeout  string `json:"timeout,omitempty"`
	Retries  *int   `json:"retries,omitempty"`
	CacheTTL string `json:"cache_ttl,omitempty"`
}

// Keys lists the settable configuration keys, in a stable order.
var Keys = []string{"api_key", "base_url", "timeout", "retries", "cache_ttl"}

// New returns an empty config.
func New() *Config { return &Config{Version: 1} }

// Path is the config file path: $DS_TOOLS_CONFIG, or <user-config-dir>/ds-tools/config.json.
func Path() (string, error) {
	if p := strings.TrimSpace(os.Getenv(EnvConfig)); p != "" {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("无法定位用户配置目录：%w", err)
	}
	return filepath.Join(dir, "ds-tools", "config.json"), nil
}

// CacheDir is the cache directory, kept next to the config file.
func CacheDir() (string, error) {
	p, err := Path()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(p), "cache"), nil
}

// Load reads the config file. A missing file is not an error: it yields an empty config.
func Load() (*Config, string, error) {
	path, err := Path()
	if err != nil {
		return nil, "", err
	}
	return LoadFrom(path)
}

// LoadFrom reads a specific config file path.
func LoadFrom(path string) (*Config, string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return New(), path, nil
		}
		return nil, path, fmt.Errorf("读取配置失败：%w", err)
	}
	cfg := New()
	if len(strings.TrimSpace(string(b))) == 0 {
		return cfg, path, nil
	}
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, path, fmt.Errorf("解析配置 %s 失败：%w", path, err)
	}
	if cfg.Version == 0 {
		cfg.Version = 1
	}
	return cfg, path, nil
}

// Save writes the config atomically with 0600 permissions.
func (c *Config) Save(path string) error {
	if c.Version == 0 {
		c.Version = 1
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("创建配置目录失败：%w", err)
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(dir, ".config-*.json")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return os.Chmod(path, 0o600)
}

// Set validates and assigns one key.
func (c *Config) Set(key, value string) error {
	switch key {
	case "api_key":
		v := strings.TrimSpace(value)
		if v == "" {
			return errors.New("api_key 不能为空")
		}
		c.APIKey = v
	case "base_url":
		v := strings.TrimSpace(value)
		u, err := url.Parse(v)
		if err != nil || u.Scheme == "" || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("base_url 必须是绝对的 http(s) 地址，当前为 %q", value)
		}
		c.BaseURL = strings.TrimRight(v, "/")
	case "timeout":
		d, err := time.ParseDuration(value)
		if err != nil || d <= 0 {
			return fmt.Errorf("timeout 必须是正的时间长度（如 30s），当前为 %q", value)
		}
		c.Timeout = d.String()
	case "retries":
		n, err := strconv.Atoi(value)
		if err != nil || n < 0 {
			return fmt.Errorf("retries 必须是非负整数，当前为 %q", value)
		}
		c.Retries = &n
	case "cache_ttl":
		d, err := time.ParseDuration(value)
		if err != nil || d < 0 {
			return fmt.Errorf("cache_ttl 必须是时间长度（如 1h 或 0s），当前为 %q", value)
		}
		c.CacheTTL = d.String()
	default:
		return fmt.Errorf("未知配置项 %q（可选：%s）", key, strings.Join(Keys, "、"))
	}
	return nil
}

// Unset removes one key.
func (c *Config) Unset(key string) error {
	switch key {
	case "api_key":
		c.APIKey = ""
	case "base_url":
		c.BaseURL = ""
	case "timeout":
		c.Timeout = ""
	case "retries":
		c.Retries = nil
	case "cache_ttl":
		c.CacheTTL = ""
	default:
		return fmt.Errorf("未知配置项 %q（可选：%s）", key, strings.Join(Keys, "、"))
	}
	return nil
}

// ResolvedTimeouts returns (timeout, retries, cacheTTL) with defaults applied.
func (c *Config) ResolvedTimeouts() (time.Duration, int, time.Duration) {
	timeout, retries, ttl := DefaultTimeout, DefaultRetries, DefaultCacheTTL
	if c == nil {
		return timeout, retries, ttl
	}
	if d, err := time.ParseDuration(c.Timeout); err == nil && d > 0 {
		timeout = d
	}
	if c.Retries != nil {
		retries = *c.Retries
	}
	if d, err := time.ParseDuration(c.CacheTTL); err == nil {
		ttl = d
	}
	return timeout, retries, ttl
}

// Mask renders an API key without leaking the secret: sk-2723…7f9e.
func Mask(key string) string {
	if key == "" {
		return "(none)"
	}
	if len(key) <= 10 {
		return "****"
	}
	return key[:6] + "…" + key[len(key)-4:]
}

// PermWarning reports whether the config file is readable by group/others.
func PermWarning(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("配置文件尚不存在")
		}
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("配置文件权限为 %#o，应为 0600（执行：chmod 600 %s）", info.Mode().Perm(), path)
	}
	return nil
}

// NonZeroKeys lists the keys currently set in the config file.
func (c *Config) NonZeroKeys() []string {
	var out []string
	if c == nil {
		return out
	}
	if c.APIKey != "" {
		out = append(out, "api_key")
	}
	if c.BaseURL != "" {
		out = append(out, "base_url")
	}
	if c.Timeout != "" {
		out = append(out, "timeout")
	}
	if c.Retries != nil {
		out = append(out, "retries")
	}
	if c.CacheTTL != "" {
		out = append(out, "cache_ttl")
	}
	sort.Strings(out)
	return out
}
