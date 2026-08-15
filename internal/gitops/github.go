package gitops

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v88/github"
)

// ErrCredentialUnavailable is what every caller sees when the App key cannot be
// read. The daemon keeps serving reads; deploys fail closed with this error.
var ErrCredentialUnavailable = errors.New("github app credential unavailable")

// branchPrefix namespaces every branch this daemon authors, so the CI path
// guard and a human reader can both tell them apart at a glance.
const branchPrefix = "snowdeploy/"

// mergeMethod keeps main's history one commit per deploy.
const mergeMethod = "squash"

// PRClient is the pull-request half of the merge-first lifecycle.
type PRClient interface {
	OpenManifestPR(ctx context.Context, service string, newContent []byte, title, body string) (int, error)
	WaitChecks(ctx context.Context, prNumber int, poll time.Duration) (bool, string, error)
	Merge(ctx context.Context, prNumber int) (string, error)
	// UpdateBranch brings the pull request's branch up to date with its base.
	// The base moving between checks and merge is a normal event under strict
	// branch protection, not an error: the caller updates, re-waits the
	// checks, and merges again.
	UpdateBranch(ctx context.Context, prNumber int) error
	ClosePR(ctx context.Context, prNumber int, comment string) error
}

// GitHubAppConfig identifies the App installation allowed to author manifest
// pull requests. BaseURL is for tests and self-hosted instances.
type GitHubAppConfig struct {
	AppID          int64
	InstallationID int64
	KeyPath        string
	Owner          string
	Repo           string
	ManifestDir    string
	BaseBranch     string
	BaseURL        string
}

type appClient struct {
	cfg GitHubAppConfig
}

// NewGitHubApp returns the PR client and the installation-token function the
// repository mirror shares. Neither reads the private key here: the key is
// read fresh per operation so a sealed secret store surfaces as a clear
// runtime error rather than a daemon that refuses to start.
func NewGitHubApp(cfg GitHubAppConfig) (PRClient, TokenFunc, error) {
	switch {
	case cfg.AppID == 0:
		return nil, nil, errors.New("github app id is required")
	case cfg.InstallationID == 0:
		return nil, nil, errors.New("github installation id is required")
	case cfg.KeyPath == "":
		return nil, nil, errors.New("github app key file is required")
	case cfg.Owner == "":
		return nil, nil, errors.New("github owner is required")
	case cfg.Repo == "":
		return nil, nil, errors.New("github repository is required")
	}
	if cfg.BaseBranch == "" {
		cfg.BaseBranch = "main"
	}
	if cfg.ManifestDir == "" {
		cfg.ManifestDir = "deploy/manifests"
	}

	a := &appClient{cfg: cfg}
	return a, a.token, nil
}

// transport builds a fresh App transport, reading the key from disk each time.
func (a *appClient) transport() (*ghinstallation.Transport, error) {
	key, err := os.ReadFile(a.cfg.KeyPath) // #nosec G304 -- operator-configured path
	if err != nil {
		return nil, fmt.Errorf(
			"%w: cannot read %s (secret store sealed?): %w",
			ErrCredentialUnavailable, a.cfg.KeyPath, err)
	}
	tr, err := ghinstallation.New(
		http.DefaultTransport, a.cfg.AppID, a.cfg.InstallationID, key)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCredentialUnavailable, err)
	}
	if a.cfg.BaseURL != "" {
		tr.BaseURL = strings.TrimSuffix(a.cfg.BaseURL, "/")
	}
	return tr, nil
}

// token mints a short-lived installation token. Nothing persists it.
func (a *appClient) token(ctx context.Context) (string, error) {
	tr, err := a.transport()
	if err != nil {
		return "", err
	}
	tok, err := tr.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: mint installation token: %w", ErrCredentialUnavailable, err)
	}
	return tok, nil
}

func (a *appClient) client() (*github.Client, error) {
	tr, err := a.transport()
	if err != nil {
		return nil, err
	}

	opts := []github.ClientOptionsFunc{github.WithTransport(tr)}
	if a.cfg.BaseURL != "" {
		base := strings.TrimSuffix(a.cfg.BaseURL, "/") + "/api/v3/"
		opts = append(opts, github.WithEnterpriseURLs(base, base))
	}

	c, err := github.NewClient(opts...)
	if err != nil {
		return nil, fmt.Errorf("build github client: %w", err)
	}
	return c, nil
}

// manifestPath is the only path this client may ever write.
func (a *appClient) manifestPath(service string) (string, error) {
	if err := checkName(service); err != nil {
		return "", err
	}
	return path.Join(a.cfg.ManifestDir, service+".yaml"), nil
}

// branchFor is derived from the content, so re-proposing the same change
// reuses the same branch name instead of littering the repository.
func branchFor(service string, content []byte) string {
	sum := sha256.Sum256(content)
	return branchPrefix + service + "-" + hex.EncodeToString(sum[:])[:12]
}

// OpenManifestPR branches from the base head, writes exactly one manifest, and
// opens the pull request. It never touches a second path.
func (a *appClient) OpenManifestPR(
	ctx context.Context, service string, newContent []byte, title, body string,
) (int, error) {
	filePath, err := a.manifestPath(service)
	if err != nil {
		return 0, err
	}
	c, err := a.client()
	if err != nil {
		return 0, err
	}

	baseRef, _, err := c.Git.GetRef(ctx, a.cfg.Owner, a.cfg.Repo, "heads/"+a.cfg.BaseBranch)
	if err != nil {
		return 0, fmt.Errorf("read base ref %s: %w", a.cfg.BaseBranch, err)
	}
	baseSHA := baseRef.GetObject().GetSHA()

	branch := branchFor(service, newContent)
	_, _, err = c.Git.CreateRef(ctx, a.cfg.Owner, a.cfg.Repo, github.CreateRef{
		Ref: "refs/heads/" + branch,
		SHA: baseSHA,
	})
	if err != nil && !isAlreadyExists(err) {
		return 0, fmt.Errorf("create branch %s: %w", branch, err)
	}

	existingSHA, err := a.blobSHA(ctx, c, filePath, branch)
	if err != nil {
		return 0, err
	}
	opts := &github.RepositoryContentFileOptions{
		Message: github.Ptr(title),
		Content: newContent,
		Branch:  github.Ptr(branch),
	}
	if existingSHA != "" {
		opts.SHA = github.Ptr(existingSHA)
	}
	if _, _, err := c.Repositories.CreateFile(
		ctx, a.cfg.Owner, a.cfg.Repo, filePath, opts); err != nil {
		return 0, fmt.Errorf("write %s: %w", filePath, err)
	}

	pr, _, err := c.PullRequests.Create(ctx, a.cfg.Owner, a.cfg.Repo, &github.NewPullRequest{
		Title: github.Ptr(title),
		Head:  github.Ptr(branch),
		Base:  github.Ptr(a.cfg.BaseBranch),
		Body:  github.Ptr(body),
	})
	if err != nil {
		// A deploy retried after a failed merge finds its own earlier pull
		// request still open on this same content-derived branch. That
		// proposal is byte-identical to the one being made, so adopt it
		// rather than failing the deploy over its own leftovers.
		if isAlreadyExists(err) {
			if adopted, findErr := a.openPRForBranch(ctx, c, branch); findErr == nil && adopted != 0 {
				return adopted, nil
			}
		}
		return 0, fmt.Errorf("open pull request: %w", err)
	}
	return pr.GetNumber(), nil
}

func (a *appClient) openPRForBranch(
	ctx context.Context, c *github.Client, branch string,
) (int, error) {
	prs, _, err := c.PullRequests.List(ctx, a.cfg.Owner, a.cfg.Repo,
		&github.PullRequestListOptions{
			State: "open",
			Head:  a.cfg.Owner + ":" + branch,
			Base:  a.cfg.BaseBranch,
		})
	if err != nil {
		return 0, fmt.Errorf("list open pull requests for %s: %w", branch, err)
	}
	if len(prs) == 0 {
		return 0, nil
	}
	return prs[0].GetNumber(), nil
}

// UpdateBranch merges the base into the pull request's branch. GitHub answers
// 202 and does the update asynchronously; "already up to date" style refusals
// are success for the caller's purpose.
func (a *appClient) UpdateBranch(ctx context.Context, prNumber int) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	_, _, err = c.PullRequests.UpdateBranch(ctx, a.cfg.Owner, a.cfg.Repo, prNumber, nil)
	if err != nil {
		// go-github surfaces the intentional 202 as AcceptedError.
		var accepted *github.AcceptedError
		if errors.As(err, &accepted) {
			return nil
		}
		return fmt.Errorf("update branch of pull request %d: %w", prNumber, err)
	}
	return nil
}

func (a *appClient) blobSHA(
	ctx context.Context, c *github.Client, filePath, branch string,
) (string, error) {
	file, _, resp, err := c.Repositories.GetContents(
		ctx, a.cfg.Owner, a.cfg.Repo, filePath, &github.RepositoryContentGetOptions{Ref: branch})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", filePath, err)
	}
	if file == nil {
		return "", nil
	}
	return file.GetSHA(), nil
}

// WaitChecks polls the head commit's check runs until they all conclude. It
// returns ok=false with a detail naming the first failure, so nothing merges
// on a red `verify`.
func (a *appClient) WaitChecks(
	ctx context.Context, prNumber int, poll time.Duration,
) (bool, string, error) {
	if poll <= 0 {
		poll = 15 * time.Second
	}
	c, err := a.client()
	if err != nil {
		return false, "", err
	}

	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		pr, _, err := c.PullRequests.Get(ctx, a.cfg.Owner, a.cfg.Repo, prNumber)
		if err != nil {
			return false, "", fmt.Errorf("read pull request %d: %w", prNumber, err)
		}
		headSHA := pr.GetHead().GetSHA()

		runs, _, err := c.Checks.ListCheckRunsForRef(
			ctx, a.cfg.Owner, a.cfg.Repo, headSHA,
			&github.ListCheckRunsOptions{ListOptions: github.ListOptions{PerPage: 100}})
		if err != nil {
			return false, "", fmt.Errorf("list check runs for %s: %w", headSHA, err)
		}

		done, ok, detail := summarizeChecks(runs)
		if done {
			return ok, detail, nil
		}

		select {
		case <-ctx.Done():
			return false, "checks did not conclude in time", fmt.Errorf(
				"wait for checks on pull request %d: %w", prNumber, ctx.Err())
		case <-ticker.C:
		}
	}
}

// summarizeChecks reports whether every run concluded, and whether they all
// concluded acceptably.
func summarizeChecks(runs *github.ListCheckRunsResults) (done, ok bool, detail string) {
	if runs == nil || runs.GetTotal() == 0 || len(runs.CheckRuns) == 0 {
		return false, false, "no checks reported yet"
	}
	for _, run := range runs.CheckRuns {
		if run.GetStatus() != "completed" {
			return false, false, fmt.Sprintf("check %q is %s", run.GetName(), run.GetStatus())
		}
		switch run.GetConclusion() {
		case "success", "neutral", "skipped":
		default:
			return true, false, fmt.Sprintf(
				"check %q concluded %s", run.GetName(), run.GetConclusion())
		}
	}
	return true, true, "all checks passed"
}

// Merge squashes the pull request and returns the merge commit.
func (a *appClient) Merge(ctx context.Context, prNumber int) (string, error) {
	c, err := a.client()
	if err != nil {
		return "", err
	}
	res, _, err := c.PullRequests.Merge(ctx, a.cfg.Owner, a.cfg.Repo, prNumber, "",
		&github.PullRequestOptions{MergeMethod: mergeMethod})
	if err != nil {
		return "", fmt.Errorf("merge pull request %d: %w", prNumber, err)
	}
	if !res.GetMerged() {
		return "", fmt.Errorf("pull request %d did not merge: %s", prNumber, res.GetMessage())
	}
	return res.GetSHA(), nil
}

// ClosePR records why a proposal was abandoned, then closes it.
func (a *appClient) ClosePR(ctx context.Context, prNumber int, comment string) error {
	c, err := a.client()
	if err != nil {
		return err
	}
	if comment != "" {
		_, _, err := c.Issues.CreateComment(ctx, a.cfg.Owner, a.cfg.Repo, prNumber,
			&github.IssueComment{Body: github.Ptr(comment)})
		if err != nil {
			return fmt.Errorf("comment on pull request %d: %w", prNumber, err)
		}
	}
	_, _, err = c.PullRequests.Edit(ctx, a.cfg.Owner, a.cfg.Repo, prNumber,
		&github.PullRequest{State: github.Ptr("closed")})
	if err != nil {
		return fmt.Errorf("close pull request %d: %w", prNumber, err)
	}
	return nil
}

func isAlreadyExists(err error) bool {
	var gerr *github.ErrorResponse
	if !errors.As(err, &gerr) {
		return false
	}
	return gerr.Response != nil &&
		gerr.Response.StatusCode == http.StatusUnprocessableEntity &&
		strings.Contains(strings.ToLower(gerr.Message), "already exists")
}
