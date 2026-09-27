package cli

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

// LocalConfig lives on the laptop: server contexts and folder → app links.
// It holds no app configuration (that lives on the server).
type LocalConfig struct {
	Current string                  `json:"current,omitempty"`
	Servers map[string]ServerConfig `json:"servers"`
	Links   map[string]Link         `json:"links"`
	path    string
}

type ServerConfig struct {
	Target string `json:"target"`        // user@host, ssh alias, or unix:/path/to.sock
	Bin    string `json:"bin,omitempty"` // remote dokwalt binary
}

type Link struct {
	Server string `json:"server"`
	App    string `json:"app"`
}

func configPath() string {
	if p := os.Getenv("DOKWALT_CONFIG"); p != "" {
		return p
	}
	dir, err := os.UserConfigDir()
	if err != nil || strings.Contains(dir, "Library/Application Support") {
		// ~/.config on macOS too: friendlier for dotfiles.
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".config")
	}
	return filepath.Join(dir, "dokwalt", "config.json")
}

func loadConfig() (*LocalConfig, error) {
	c := &LocalConfig{Servers: map[string]ServerConfig{}, Links: map[string]Link{}, path: configPath()}
	b, err := os.ReadFile(c.path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, err
	}
	if c.Servers == nil {
		c.Servers = map[string]ServerConfig{}
	}
	if c.Links == nil {
		c.Links = map[string]Link{}
	}
	return c, nil
}

func (c *LocalConfig) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// LinkFor finds the link of dir or its closest parent.
func (c *LocalConfig) LinkFor(dir string) (Link, string, bool) {
	dir, _ = filepath.Abs(dir)
	for {
		if l, ok := c.Links[dir]; ok {
			return l, dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return Link{}, "", false
		}
		dir = parent
	}
}
