package gitops

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const fixtureManifest = `name: web
image:
  repository: registry.example.com/acme/web
  digest: sha256:0000000000000000000000000000000000000000000000000000000000000abc
template: web.container.tmpl
port: 8080
network: acme
health:
  url: http://127.0.0.1:8080/healthz
`

const fixtureTemplate = "[Container]\nImage={{.Image.Repository}}@{{.Image.Digest}}\n"

// fixtureRemote builds a bare repository with one manifest and one template,
// and returns its path plus a function that commits further changes to it.
func fixtureRemote(t *testing.T) (remote string, push func(relPath, content string)) {
	t.Helper()

	root := t.TempDir()
	remote = filepath.Join(root, "remote.git")
	work := filepath.Join(root, "seed")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	run(root, "init", "--bare", "-b", "main", remote)
	run(root, "clone", remote, work)

	write := func(relPath, content string) {
		t.Helper()
		full := filepath.Join(work, relPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("deploy/manifests/web.yaml", fixtureManifest)
	write("deploy/templates/web.container.tmpl", fixtureTemplate)
	run(work, "add", "-A")
	run(work, "commit", "-m", "seed")
	run(work, "push", "origin", "main")

	push = func(relPath, content string) {
		t.Helper()
		write(relPath, content)
		run(work, "add", "-A")
		run(work, "commit", "-m", "update "+relPath)
		run(work, "push", "origin", "main")
	}
	return remote, push
}

func newTestRepo(t *testing.T, remote string) *Repo {
	t.Helper()
	return NewRepo(RepoConfig{
		URL:         remote,
		Branch:      "main",
		CacheDir:    filepath.Join(t.TempDir(), "cache"),
		ManifestDir: "deploy/manifests",
		TemplateDir: "deploy/templates",
	})
}

func TestSyncClonesThenFastForwards(t *testing.T) {
	remote, push := fixtureRemote(t)
	r := newTestRepo(t, remote)

	first, err := r.Sync(t.Context())
	if err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	if len(first) != 40 {
		t.Fatalf("Sync returned %q, want a 40-char SHA", first)
	}

	m, _, err := r.Manifest("web")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.Image.Digest != "sha256:0000000000000000000000000000000000000000000000000000000000000abc" {
		t.Errorf("digest = %q", m.Image.Digest)
	}

	push("deploy/manifests/web.yaml",
		strings.Replace(fixtureManifest, "000abc", "000def", 1))

	second, err := r.Sync(t.Context())
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if second == first {
		t.Fatal("Sync did not advance to the new head")
	}

	m, raw, err := r.Manifest("web")
	if err != nil {
		t.Fatalf("Manifest after sync: %v", err)
	}
	if !strings.HasSuffix(m.Image.Digest, "000def") {
		t.Errorf("digest after sync = %q, want the pushed value", m.Image.Digest)
	}
	if !strings.Contains(string(raw), "000def") {
		t.Errorf("raw bytes are stale: %s", raw)
	}
}

func TestSyncDiscardsLocalEdits(t *testing.T) {
	remote, _ := fixtureRemote(t)
	r := newTestRepo(t, remote)

	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	tampered := filepath.Join(r.CacheDir(), "deploy/manifests/web.yaml")
	if err := os.WriteFile(tampered, []byte("name: tampered\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatalf("re-Sync: %v", err)
	}
	m, _, err := r.Manifest("web")
	if err != nil {
		t.Fatalf("Manifest: %v", err)
	}
	if m.Name != "web" {
		t.Fatalf("local tampering survived Sync: name = %q", m.Name)
	}
}

func TestTemplateReturnsCommittedBytes(t *testing.T) {
	remote, _ := fixtureRemote(t)
	r := newTestRepo(t, remote)
	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}

	got, err := r.Template("web.container.tmpl")
	if err != nil {
		t.Fatalf("Template: %v", err)
	}
	if got != fixtureTemplate {
		t.Errorf("Template = %q, want %q", got, fixtureTemplate)
	}
}

func TestServicesListsManifests(t *testing.T) {
	remote, push := fixtureRemote(t)
	r := newTestRepo(t, remote)
	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	push("deploy/manifests/api.yaml", strings.Replace(fixtureManifest, "name: web", "name: api", 1))
	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}

	got, err := r.Services()
	if err != nil {
		t.Fatalf("Services: %v", err)
	}
	if len(got) != 2 || got[0] != "api" || got[1] != "web" {
		t.Errorf("Services = %v, want [api web]", got)
	}
}

func TestPathTraversalIsRejected(t *testing.T) {
	remote, _ := fixtureRemote(t)
	r := newTestRepo(t, remote)
	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}

	for _, bad := range []string{"../../etc/passwd", "a/b", "", ".", ".."} {
		if _, _, err := r.Manifest(bad); err == nil {
			t.Errorf("Manifest(%q) accepted", bad)
		}
		if _, err := r.Template(bad); err == nil {
			t.Errorf("Template(%q) accepted", bad)
		}
	}
}

func TestManifestPathIsRepoRelative(t *testing.T) {
	remote, _ := fixtureRemote(t)
	r := newTestRepo(t, remote)
	if _, err := r.Sync(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := r.ManifestPath("web"); got != "deploy/manifests/web.yaml" {
		t.Errorf("ManifestPath = %q", got)
	}
}

func TestManifestBeforeSync(t *testing.T) {
	remote, _ := fixtureRemote(t)
	r := newTestRepo(t, remote)
	if _, _, err := r.Manifest("web"); err == nil {
		t.Fatal("Manifest succeeded before any Sync")
	}
}
