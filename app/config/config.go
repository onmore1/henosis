// Package config is what the Henosis app remembers on this computer: the
// server, the account, and which folders are linked to which projects.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// DefaultPort is where the status page listens on 127.0.0.1.
const DefaultPort = 47800

// Link ties a project on the server to a folder on this computer.
type Link struct {
	ProjectID string `json:"project_id"`
	Name      string `json:"name"`
	Folder    string `json:"folder"`
}

// Config is the content of config.json.
type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
	User   string `json:"user"`
	UserID string `json:"user_id"`
	Device string `json:"device"` // names this computer; locks belong to a user on a device
	Port   int    `json:"status_port,omitempty"`
	Links  []Link `json:"links"`
	Host   Host   `json:"host"`
	// AutoLink puts every project of the session on this computer by itself.
	AutoLink bool `json:"auto_link,omitempty"`
	// Skipped are projects the user took off this computer on purpose.
	Skipped []string `json:"skipped,omitempty"`

	path string
}

// Host says whether this computer also runs the server, inside the app.
type Host struct {
	Enabled bool `json:"enabled"`
	Port    int  `json:"port,omitempty"`
	// JoinSecret is the secret inside the session code (16 bytes as hex).
	JoinSecret string `json:"join_secret,omitempty"`
	// Extra is an address the host added to the code by hand.
	Extra string `json:"extra,omitempty"`
}

// DefaultServerPort is where a hosted server listens.
const DefaultServerPort = 8787

// DefaultPath is %AppData%\Henosis\config.json on Windows. The environment
// variable HENOSIS_CONFIG overrides it.
func DefaultPath() string {
	if path := os.Getenv("HENOSIS_CONFIG"); path != "" {
		return path
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "Henosis", "config.json")
}

// Load reads the config. A missing file gives an empty config.
func Load(path string) (*Config, error) {
	c := &Config{path: path}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s is damaged: %w", path, err)
	}
	return c, nil
}

// Path is where this config is stored.
func (c *Config) Path() string { return c.path }

// Dir is the folder that holds the config and the app's log.
func (c *Config) Dir() string { return filepath.Dir(c.path) }

// Save writes the config, readable by this user only (it holds the token).
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".new"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// SignedIn reports whether a server and a token are stored.
func (c *Config) SignedIn() bool { return c.Server != "" && c.Token != "" }

// StatusPort is the port of the status page.
func (c *Config) StatusPort() int {
	if c.Port > 0 {
		return c.Port
	}
	return DefaultPort
}

// NewDevice makes a name for this computer: the host name plus a short
// random part, so two computers with the same name stay distinct.
func NewDevice() string {
	host, _ := os.Hostname()
	var clean strings.Builder
	for _, r := range strings.ToLower(host) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			clean.WriteRune(r)
		}
	}
	name := clean.String()
	if name == "" {
		name = "pc"
	}
	if len(name) > 24 {
		name = name[:24]
	}
	b := make([]byte, 2)
	_, _ = rand.Read(b)
	return name + "-" + hex.EncodeToString(b)
}

// CleanFolder turns a folder the user typed into an absolute, clean path.
func CleanFolder(folder string) (string, error) {
	folder = strings.TrimSpace(strings.Trim(strings.TrimSpace(folder), `"`))
	if folder == "" {
		return "", errors.New("a folder is required")
	}
	abs, err := filepath.Abs(folder)
	if err != nil {
		return "", err
	}
	return filepath.Clean(abs), nil
}

// key is a folder path in a form that can be compared.
func key(folder string) string {
	folder = filepath.Clean(folder)
	if runtime.GOOS == "windows" {
		folder = strings.ToLower(folder)
	}
	return folder
}

// within reports whether path is inside dir or equal to it.
func within(path, dir string) bool {
	path, dir = key(path), key(dir)
	if path == dir {
		return true
	}
	if !strings.HasSuffix(dir, string(filepath.Separator)) {
		dir += string(filepath.Separator)
	}
	return strings.HasPrefix(path, dir)
}

// LinkFor finds the link whose folder is the given folder or contains it.
func (c *Config) LinkFor(folder string) *Link {
	for i := range c.Links {
		if within(folder, c.Links[i].Folder) {
			return &c.Links[i]
		}
	}
	return nil
}

// LinkByProject finds the link of a project.
func (c *Config) LinkByProject(projectID string) *Link {
	for i := range c.Links {
		if c.Links[i].ProjectID == projectID {
			return &c.Links[i]
		}
	}
	return nil
}

// CheckFolder says whether a folder may be linked: it must not be a drive
// root or the home folder (everything in a linked folder is uploaded), and
// it must not overlap a folder that is already linked.
func (c *Config) CheckFolder(folder string) error {
	if filepath.Dir(folder) == folder {
		return errors.New("a whole drive cannot be a project folder")
	}
	if home, err := os.UserHomeDir(); err == nil && within(home, folder) {
		return errors.New("that folder contains your whole user folder; pick the folder of one project")
	}
	for _, link := range c.Links {
		if within(folder, link.Folder) || within(link.Folder, folder) {
			return fmt.Errorf("that folder overlaps %q, which is already linked to %q", link.Folder, link.Name)
		}
	}
	return nil
}

// AddLink links a project to a folder.
func (c *Config) AddLink(link Link) error {
	if c.LinkByProject(link.ProjectID) != nil {
		return fmt.Errorf("%q is already linked to %s", link.Name, c.LinkByProject(link.ProjectID).Folder)
	}
	if err := c.CheckFolder(link.Folder); err != nil {
		return err
	}
	c.Links = append(c.Links, link)
	return nil
}

// RemoveLink forgets the link of a project. The folder and its files stay.
func (c *Config) RemoveLink(projectID string) bool {
	for i, link := range c.Links {
		if link.ProjectID == projectID {
			c.Links = append(c.Links[:i], c.Links[i+1:]...)
			return true
		}
	}
	return false
}
