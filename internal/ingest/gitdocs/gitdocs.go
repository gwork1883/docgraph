package gitdocs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Config struct {
	Branch string `json:"branch"`
	Path   string `json:"path"`
	Cache  string `json:"cache"`
}

type Checkout struct {
	Root string
	URL  string
}

func Sync(ctx context.Context, repoURL string, configJSON string) (Checkout, error) {
	repoURL = strings.TrimSpace(repoURL)
	if repoURL == "" {
		return Checkout{}, fmt.Errorf("git source dsn is required")
	}
	var cfg Config
	if strings.TrimSpace(configJSON) != "" {
		if err := json.Unmarshal([]byte(configJSON), &cfg); err != nil {
			return Checkout{}, fmt.Errorf("parse git source config_json: %w", err)
		}
	}
	cfg.Branch = strings.TrimSpace(cfg.Branch)
	cfg.Path = strings.Trim(filepath.Clean(strings.TrimSpace(cfg.Path)), string(filepath.Separator))
	cfg.Cache = strings.TrimSpace(cfg.Cache)

	if cfg.Cache == "" {
		return syncLocalCheckout(ctx, repoURL, cfg)
	}
	return syncCachedCheckout(ctx, repoURL, cfg)
}

func syncLocalCheckout(ctx context.Context, repoURL string, cfg Config) (Checkout, error) {
	repoRoot, err := localRepoPath(repoURL)
	if err != nil {
		return Checkout{}, err
	}
	if err := ensureGitWorktree(ctx, repoRoot); err != nil {
		return Checkout{}, err
	}
	if cfg.Branch != "" {
		currentBranch, err := gitOutput(ctx, repoRoot, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			return Checkout{}, fmt.Errorf("read current git branch: %w", err)
		}
		if strings.TrimSpace(currentBranch) != cfg.Branch {
			return Checkout{}, fmt.Errorf("git source branch is %q, want %q; checkout the branch locally or configure cache explicitly", strings.TrimSpace(currentBranch), cfg.Branch)
		}
	}
	return validateCheckoutRoot(repoRoot, repoURL, cfg.Path, false)
}

func syncCachedCheckout(ctx context.Context, repoURL string, cfg Config) (Checkout, error) {
	if _, err := os.Stat(filepath.Join(cfg.Cache, ".git")); err != nil {
		if err := runGit(ctx, "", "clone", "--no-tags", repoURL, cfg.Cache); err != nil {
			return Checkout{}, err
		}
	} else if err := runGit(ctx, cfg.Cache, "fetch", "--prune", "origin"); err != nil {
		return Checkout{}, err
	}
	if cfg.Branch != "" {
		if err := checkoutBranch(ctx, cfg.Cache, cfg.Branch); err != nil {
			return Checkout{}, err
		}
	} else if err := runGit(ctx, cfg.Cache, "pull", "--ff-only"); err != nil {
		return Checkout{}, err
	}
	return validateCheckoutRoot(cfg.Cache, repoURL, cfg.Path, true)
}

func validateCheckoutRoot(repoRoot string, repoURL string, sourcePath string, allowCreateMissing bool) (Checkout, error) {
	root := repoRoot
	urlPrefix := repoURL
	if sourcePath != "" && sourcePath != "." {
		root = filepath.Join(repoRoot, sourcePath)
		urlPrefix = strings.TrimRight(repoURL, "/") + "/" + filepath.ToSlash(sourcePath)
	}
	info, err := os.Stat(root)
	if err != nil {
		if allowCreateMissing && os.IsNotExist(err) && sourcePath != "" {
			if err := os.MkdirAll(root, 0o755); err != nil {
				return Checkout{}, fmt.Errorf("create empty git source path %q: %w", sourcePath, err)
			}
			return Checkout{Root: root, URL: urlPrefix}, nil
		}
		return Checkout{}, fmt.Errorf("git source path %q: %w", sourcePath, err)
	}
	if !info.IsDir() {
		return Checkout{}, fmt.Errorf("git source path %q is not a directory", sourcePath)
	}
	return Checkout{Root: root, URL: urlPrefix}, nil
}

func localRepoPath(repoURL string) (string, error) {
	if strings.HasPrefix(repoURL, "file://") {
		path := strings.TrimPrefix(repoURL, "file://")
		if strings.TrimSpace(path) == "" {
			return "", fmt.Errorf("git source dsn file:// path is empty")
		}
		return filepath.Clean(path), nil
	}
	if strings.Contains(repoURL, "://") || strings.HasPrefix(repoURL, "git@") {
		return "", fmt.Errorf("git source dsn %q is remote; configure cache explicitly to clone remote Git sources", repoURL)
	}
	return filepath.Clean(repoURL), nil
}

func ensureGitWorktree(ctx context.Context, dir string) error {
	if err := runGit(ctx, dir, "rev-parse", "--is-inside-work-tree"); err != nil {
		return fmt.Errorf("git source dsn %q is not a Git worktree: %w", dir, err)
	}
	return nil
}

func checkoutBranch(ctx context.Context, dir string, branch string) error {
	if err := runGit(ctx, dir, "checkout", branch); err != nil {
		if err := runGit(ctx, dir, "checkout", "-B", branch, "origin/"+branch); err != nil {
			return err
		}
	}
	return runGit(ctx, dir, "pull", "--ff-only", "origin", branch)
}

func runGit(ctx context.Context, dir string, args ...string) error {
	_, err := gitOutput(ctx, dir, args...)
	return err
}

func gitOutput(ctx context.Context, dir string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}
