// Package gitops owns the two Git-shaped halves of the merge-first lifecycle:
// keeping a local mirror of the merged configuration branch, and authoring the
// pull requests that change it.
package gitops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/SnowballSH/snowdeploy/internal/manifest"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// TokenFunc mints a short-lived credential. It is called per operation and its
// result is never persisted.
type TokenFunc func(ctx context.Context) (string, error)

// RepoConfig describes the configuration repository holding the manifests.
type RepoConfig struct {
	URL         string
	Branch      string
	CacheDir    string
	ManifestDir string
	TemplateDir string
	TokenFn     TokenFunc
}

// Repo is a local mirror of the merged configuration branch. Nothing here ever
// writes to the remote: the host only reads what main already says.
type Repo struct {
	cfg RepoConfig

	mu     sync.RWMutex
	synced bool
}

// NewRepo builds a mirror without touching the network.
func NewRepo(cfg RepoConfig) *Repo {
	if cfg.Branch == "" {
		cfg.Branch = "main"
	}
	return &Repo{cfg: cfg}
}

// CacheDir is the working tree the mirror is kept in.
func (r *Repo) CacheDir() string { return r.cfg.CacheDir }

// ManifestPath is a service's manifest path relative to the repository root.
func (r *Repo) ManifestPath(service string) string {
	return path.Join(r.cfg.ManifestDir, service+".yaml")
}

// Sync brings the mirror to the remote branch head and returns that commit.
// The working tree is reset hard, so local drift can never be read as state.
func (r *Repo) Sync(ctx context.Context) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	repo, err := r.openOrClone(ctx)
	if err != nil {
		return "", err
	}

	auth, err := r.auth(ctx)
	if err != nil {
		return "", err
	}
	refspec := config.RefSpec(fmt.Sprintf(
		"+refs/heads/%s:refs/remotes/origin/%s", r.cfg.Branch, r.cfg.Branch))
	err = repo.FetchContext(ctx, &git.FetchOptions{
		RemoteName: git.DefaultRemoteName,
		RefSpecs:   []config.RefSpec{refspec},
		Auth:       auth,
		Force:      true,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return "", fmt.Errorf("fetch %s: %w", r.cfg.Branch, err)
	}

	remoteRef, err := repo.Reference(
		plumbing.NewRemoteReferenceName(git.DefaultRemoteName, r.cfg.Branch), true)
	if err != nil {
		return "", fmt.Errorf("resolve origin/%s: %w", r.cfg.Branch, err)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return "", fmt.Errorf("open worktree: %w", err)
	}
	if err := wt.Reset(&git.ResetOptions{
		Commit: remoteRef.Hash(),
		Mode:   git.HardReset,
	}); err != nil {
		return "", fmt.Errorf("reset to origin/%s: %w", r.cfg.Branch, err)
	}
	if err := wt.Clean(&git.CleanOptions{Dir: true}); err != nil {
		return "", fmt.Errorf("clean worktree: %w", err)
	}

	r.synced = true
	return remoteRef.Hash().String(), nil
}

func (r *Repo) openOrClone(ctx context.Context) (*git.Repository, error) {
	repo, err := git.PlainOpen(r.cfg.CacheDir)
	if err == nil {
		return repo, nil
	}
	if !errors.Is(err, git.ErrRepositoryNotExists) {
		return nil, fmt.Errorf("open cache %s: %w", r.cfg.CacheDir, err)
	}

	auth, err := r.auth(ctx)
	if err != nil {
		return nil, err
	}
	repo, err = git.PlainCloneContext(ctx, r.cfg.CacheDir, false, &git.CloneOptions{
		URL:           r.cfg.URL,
		Auth:          auth,
		ReferenceName: plumbing.NewBranchReferenceName(r.cfg.Branch),
		SingleBranch:  true,
	})
	if err != nil {
		return nil, fmt.Errorf("clone %s: %w", r.cfg.URL, err)
	}
	return repo, nil
}

// auth supplies an installation token only for https remotes; a local or ssh
// remote must never be handed a GitHub credential.
func (r *Repo) auth(ctx context.Context) (*githttp.BasicAuth, error) {
	if r.cfg.TokenFn == nil || !strings.HasPrefix(r.cfg.URL, "http") {
		return nil, nil
	}
	token, err := r.cfg.TokenFn(ctx)
	if err != nil {
		return nil, err
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: token}, nil
}

// Manifest returns a service's parsed manifest and the exact bytes on disk.
func (r *Repo) Manifest(service string) (*manifest.Manifest, []byte, error) {
	raw, err := r.readRelative(r.cfg.ManifestDir, service+".yaml", service)
	if err != nil {
		return nil, nil, err
	}
	m, err := manifest.Parse(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("manifest %s: %w", service, err)
	}
	return m, raw, nil
}

// Template returns a Quadlet template's text.
func (r *Repo) Template(name string) (string, error) {
	raw, err := r.readRelative(r.cfg.TemplateDir, name, name)
	if err != nil {
		return "", err
	}
	return string(raw), nil
}

// Services lists the manifests present in the mirror, sorted.
func (r *Repo) Services() ([]string, error) {
	if err := r.requireSynced(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(r.cfg.CacheDir, r.cfg.ManifestDir))
	if err != nil {
		return nil, fmt.Errorf("list manifests: %w", err)
	}

	var out []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".yaml" {
			continue
		}
		out = append(out, strings.TrimSuffix(e.Name(), ".yaml"))
	}
	slices.Sort(out)
	return out, nil
}

func (r *Repo) readRelative(dir, file, nameForCheck string) ([]byte, error) {
	if err := r.requireSynced(); err != nil {
		return nil, err
	}
	if err := checkName(nameForCheck); err != nil {
		return nil, err
	}
	full := filepath.Join(r.cfg.CacheDir, dir, file)
	raw, err := os.ReadFile(full) // #nosec G304 -- name is validated by checkName
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path.Join(dir, file), err)
	}
	return raw, nil
}

func (r *Repo) requireSynced() error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if !r.synced {
		return errors.New("repository has not been synced yet")
	}
	return nil
}

// checkName keeps an API-supplied service or template name inside its
// directory: no separators, no traversal, no empties.
func checkName(name string) error {
	if name == "" {
		return errors.New("name is empty")
	}
	if name == "." || name == ".." || strings.Contains(name, "/") ||
		strings.Contains(name, `\`) || strings.Contains(name, "..") {
		return fmt.Errorf("name %q is not a plain file name", name)
	}
	return nil
}
