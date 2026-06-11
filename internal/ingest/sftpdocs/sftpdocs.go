package sftpdocs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

type Config struct {
	IdentityFile  string   `json:"identity_file"`
	Password      string   `json:"password"`
	Passphrase    string   `json:"passphrase"`
	KnownHosts    string   `json:"known_hosts"`
	StrictHostKey bool     `json:"strict_host_key"`
	Cache         string   `json:"cache"`
	URLPrefix     string   `json:"url_prefix"`
	Include       []string `json:"include"`
	Exclude       []string `json:"exclude"`
}

type Checkout struct {
	Root string
	URL  string
}

type remoteClient interface {
	Walk(root string, visit func(path string, isDir bool) error) error
	ReadFile(path string) ([]byte, error)
	Close() error
}

var connectRemote = connectSFTP

func Sync(ctx context.Context, dsn string, sourceID string, configJSON string) (Checkout, error) {
	remote, err := parseDSN(dsn)
	if err != nil {
		return Checkout{}, err
	}
	cfg, err := parseConfig(configJSON)
	if err != nil {
		return Checkout{}, err
	}
	cacheRoot := strings.TrimSpace(cfg.Cache)
	if cacheRoot == "" {
		cacheRoot = filepath.Join(".docgraph", "cache", "sftp", stableKey(sourceID, dsn))
	}
	cacheRoot = filepath.Clean(cacheRoot)
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		return Checkout{}, fmt.Errorf("create sftp cache %s: %w", cacheRoot, err)
	}

	client, err := connectRemote(ctx, remote, cfg)
	if err != nil {
		return Checkout{}, err
	}
	defer client.Close()

	keep, err := mirror(ctx, client, remote.Path, cacheRoot, cfg)
	if err != nil {
		return Checkout{}, err
	}
	if len(keep) == 0 {
		return Checkout{}, fmt.Errorf("no SFTP documents found under %s; include=%v exclude=%v", remote.Path, cfg.Include, cfg.Exclude)
	}
	if err := removeStale(cacheRoot, keep); err != nil {
		return Checkout{}, err
	}
	urlPrefix := strings.TrimSpace(cfg.URLPrefix)
	if urlPrefix == "" {
		urlPrefix = remote.WebURL()
	}
	return Checkout{Root: cacheRoot, URL: urlPrefix}, nil
}

type remoteTarget struct {
	User string
	Host string
	Port string
	Path string
}

func parseDSN(dsn string) (remoteTarget, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return remoteTarget{}, fmt.Errorf("sftp dsn is required")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return remoteTarget{}, fmt.Errorf("parse sftp dsn: %w", err)
	}
	if u.Scheme != "sftp" {
		return remoteTarget{}, fmt.Errorf("sftp dsn must use sftp:// scheme")
	}
	host := strings.TrimSpace(u.Hostname())
	if host == "" {
		return remoteTarget{}, fmt.Errorf("sftp dsn host is required")
	}
	remotePath := path.Clean("/" + strings.TrimLeft(u.Path, "/"))
	if remotePath == "/" || remotePath == "." {
		return remoteTarget{}, fmt.Errorf("sftp dsn path is required")
	}
	port := u.Port()
	if port == "" {
		port = "22"
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	if user == "" {
		user = os.Getenv("USER")
	}
	if user == "" {
		return remoteTarget{}, fmt.Errorf("sftp dsn user is required")
	}
	return remoteTarget{User: user, Host: host, Port: port, Path: remotePath}, nil
}

func (r remoteTarget) address() string {
	return net.JoinHostPort(r.Host, r.Port)
}

func (r remoteTarget) WebURL() string {
	return "sftp://" + r.User + "@" + r.address() + r.Path
}

func parseConfig(configJSON string) (Config, error) {
	cfg := Config{
		Include: []string{"**/*.md", "**/*.markdown", "**/*.html", "**/*.htm", "**/*.txt"},
		Exclude: []string{
			"**/.*/**",
			"**/node_modules/**",
			"**/vendor/**",
			"**/build/**",
			"**/dist/**",
			"**/out/**",
			"**/target/**",
			"**/coverage/**",
			"**/__pycache__/**",
		},
	}
	configJSON = strings.TrimSpace(configJSON)
	if configJSON == "" || configJSON == "{}" {
		return cfg, nil
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(configJSON), &raw); err != nil {
		return Config{}, fmt.Errorf("parse sftp config_json: %w", err)
	}
	cfg.IdentityFile = stringValue(raw["identity_file"])
	cfg.Password = stringValue(raw["password"])
	cfg.Passphrase = stringValue(raw["passphrase"])
	cfg.KnownHosts = stringValue(raw["known_hosts"])
	cfg.Cache = stringValue(raw["cache"])
	cfg.URLPrefix = stringValue(raw["url_prefix"])
	cfg.StrictHostKey = boolValue(raw["strict_host_key"])
	if include := stringList(raw["include"]); len(include) > 0 {
		cfg.Include = include
	}
	if exclude := stringList(raw["exclude"]); len(exclude) > 0 {
		cfg.Exclude = append(cfg.Exclude, exclude...)
	}
	return cfg, nil
}

func mirror(ctx context.Context, client remoteClient, remoteRoot string, cacheRoot string, cfg Config) (map[string]bool, error) {
	keep := map[string]bool{}
	err := client.Walk(remoteRoot, func(remotePath string, isDir bool) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		rel := strings.TrimPrefix(path.Clean(remotePath), path.Clean(remoteRoot))
		rel = strings.TrimLeft(rel, "/")
		if rel == "" {
			return nil
		}
		if isDir {
			return nil
		}
		if !included(rel, cfg) || excluded(rel, cfg) {
			return nil
		}
		content, err := client.ReadFile(remotePath)
		if err != nil {
			return fmt.Errorf("read sftp %s: %w", remotePath, err)
		}
		localPath := filepath.Join(cacheRoot, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(localPath, content, 0o644); err != nil {
			return fmt.Errorf("write sftp cache %s: %w", localPath, err)
		}
		keep[filepath.ToSlash(rel)] = true
		return nil
	})
	return keep, err
}

func removeStale(cacheRoot string, keep map[string]bool) error {
	return filepath.WalkDir(cacheRoot, func(filePath string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(cacheRoot, filePath)
		if err != nil {
			return err
		}
		if keep[filepath.ToSlash(rel)] {
			return nil
		}
		return os.Remove(filePath)
	})
}

func connectSFTP(ctx context.Context, remote remoteTarget, cfg Config) (remoteClient, error) {
	authMethods, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	sshConfig := &ssh.ClientConfig{
		User:            remote.User,
		Auth:            authMethods,
		HostKeyCallback: hostKeyCallback(cfg),
		Timeout:         15 * time.Second,
	}
	dialer := net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", remote.address())
	if err != nil {
		return nil, fmt.Errorf("connect sftp %s: %w", remote.address(), err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, remote.address(), sshConfig)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("ssh handshake %s: %w", remote.address(), err)
	}
	sshClient := ssh.NewClient(sshConn, chans, reqs)
	client, err := sftp.NewClient(sshClient)
	if err != nil {
		_ = sshClient.Close()
		return nil, fmt.Errorf("open sftp session: %w", err)
	}
	return &realClient{sftp: client, ssh: sshClient}, nil
}

func authMethods(cfg Config) ([]ssh.AuthMethod, error) {
	methods := make([]ssh.AuthMethod, 0)
	if cfg.Password != "" {
		methods = append(methods, ssh.Password(cfg.Password))
	}
	if cfg.IdentityFile != "" {
		key, err := os.ReadFile(expandHome(cfg.IdentityFile))
		if err != nil {
			return nil, fmt.Errorf("read identity_file %s: %w", cfg.IdentityFile, err)
		}
		var signer ssh.Signer
		if cfg.Passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(key, []byte(cfg.Passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(key)
		}
		if err != nil {
			return nil, fmt.Errorf("parse identity_file %s: %w", cfg.IdentityFile, err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			methods = append(methods, ssh.PublicKeysCallback(agent.NewClient(conn).Signers))
		}
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("sftp auth requires password, identity_file, or SSH_AUTH_SOCK")
	}
	return methods, nil
}

func hostKeyCallback(cfg Config) ssh.HostKeyCallback {
	if cfg.StrictHostKey {
		knownHosts := strings.TrimSpace(cfg.KnownHosts)
		if knownHosts == "" {
			knownHosts = "~/.ssh/known_hosts"
		}
		callback, err := knownhosts.New(expandHome(knownHosts))
		if err == nil {
			return callback
		}
	}
	return ssh.InsecureIgnoreHostKey()
}

type realClient struct {
	sftp *sftp.Client
	ssh  *ssh.Client
}

func (c *realClient) Walk(root string, visit func(path string, isDir bool) error) error {
	walker := c.sftp.Walk(root)
	for walker.Step() {
		if err := walker.Err(); err != nil {
			return err
		}
		if err := visit(walker.Path(), walker.Stat().IsDir()); err != nil {
			return err
		}
	}
	return nil
}

func (c *realClient) ReadFile(path string) ([]byte, error) {
	f, err := c.sftp.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

func (c *realClient) Close() error {
	err := c.sftp.Close()
	if sshErr := c.ssh.Close(); err == nil {
		err = sshErr
	}
	return err
}

func included(rel string, cfg Config) bool {
	return matchAny(cfg.Include, rel)
}

func excluded(rel string, cfg Config) bool {
	return matchAny(cfg.Exclude, rel)
}

func matchAny(patterns []string, rel string) bool {
	for _, pattern := range patterns {
		if matchPattern(pattern, rel) {
			return true
		}
	}
	return false
}

func matchPattern(pattern string, rel string) bool {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	rel = filepath.ToSlash(strings.TrimPrefix(strings.TrimSpace(rel), "./"))
	if pattern == "" || rel == "" {
		return false
	}
	if strings.HasPrefix(pattern, "**/") && strings.HasSuffix(pattern, "/**") {
		middle := strings.TrimSuffix(strings.TrimPrefix(pattern, "**/"), "/**")
		if rel == middle || strings.HasPrefix(rel, middle+"/") || strings.Contains(rel, "/"+middle+"/") {
			return true
		}
	}
	if strings.HasPrefix(pattern, "**/") && matchPattern(strings.TrimPrefix(pattern, "**/"), rel) {
		return true
	}
	if strings.HasSuffix(pattern, "/**") {
		prefix := strings.TrimSuffix(pattern, "/**")
		return rel == prefix || strings.HasPrefix(rel, prefix+"/")
	}
	if ok, _ := path.Match(pattern, rel); ok {
		return true
	}
	if strings.Contains(pattern, "**") {
		parts := strings.Split(pattern, "**")
		if len(parts) == 2 {
			prefix := strings.Trim(parts[0], "/")
			suffix := strings.Trim(parts[1], "/")
			if prefix != "" && !strings.HasPrefix(rel, prefix+"/") && rel != prefix {
				return false
			}
			if suffix != "" {
				if ok, _ := path.Match(suffix, path.Base(rel)); ok {
					return true
				}
				return strings.HasSuffix(rel, suffix)
			}
			return true
		}
	}
	return false
}

func stringValue(value any) string {
	if s, ok := value.(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func boolValue(value any) bool {
	if b, ok := value.(bool); ok {
		return b
	}
	return false
}

func stringList(value any) []string {
	switch typed := value.(type) {
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				values = appendSplit(values, s)
			}
		}
		return values
	case string:
		return appendSplit(nil, typed)
	default:
		return nil
	}
}

func appendSplit(values []string, raw string) []string {
	for _, part := range strings.Split(raw, ",") {
		if part = strings.TrimSpace(part); part != "" {
			values = append(values, part)
		}
	}
	return values
}

func expandHome(value string) string {
	value = strings.TrimSpace(value)
	if value == "~" || strings.HasPrefix(value, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(value, "~/"))
		}
	}
	return value
}

func stableKey(values ...string) string {
	joined := strings.Join(values, "\n")
	sum := sha256.Sum256([]byte(joined))
	return hex.EncodeToString(sum[:12])
}
