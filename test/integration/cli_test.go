package integration

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "archive-cache-integration-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "gh-actions-archive-cache")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	args := []string{"build", "-tags=integrationtest", "-o", binary}
	// Match the parent runner's cgo availability for the subprocess binary.
	if os.Getenv("CGO_ENABLED") == "1" {
		args = append(args, "-race")
	}
	args = append(args, "./cmd/gh-actions-archive-cache")
	cmd := exec.Command("go", args...)
	cmd.Dir = filepath.Join("..", "..")
	if output, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "build integration binary: %v\n%s", err, output)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

func fixtureArchive(t *testing.T, format string) []byte {
	t.Helper()
	var b bytes.Buffer
	if format == "zip" {
		w := zip.NewWriter(&b)
		if _, err := w.Create("fixture/"); err != nil {
			t.Fatal(err)
		}
		f, err := w.Create("fixture/action.yml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(f, "name: integration\n"); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		g := gzip.NewWriter(&b)
		w := tar.NewWriter(g)
		content := "name: integration\n"
		if err := w.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader,
			PAXRecords: map[string]string{"comment": "git archive fixture"}}); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteHeader(&tar.Header{Name: "fixture/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteHeader(&tar.Header{Name: "fixture/action.yml", Size: int64(len(content)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, content); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		if err := g.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

type fixture struct {
	mu       sync.Mutex
	requests []string
	archives int
	missing  bool
	offline  bool
	name     string
	zip, tar []byte
}

func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	host := r.Header.Get("X-Fixture-Host")
	f.requests = append(f.requests, host+r.URL.RequestURI())
	if f.offline {
		http.Error(w, "unexpected network request", 500)
		return
	}
	auth := r.Header.Get("Authorization")
	if host == "api.tenant.ghe.com" && auth != "token tenant-secret" ||
		host == "api.github.com" && auth != "token public-secret" {
		http.Error(w, "credential boundary violation", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if f.missing && host == "api.tenant.ghe.com" {
		http.Error(w, "not found", 404)
		return
	}
	p := r.URL.Path
	p = strings.Replace(p, "/repos/o/sub/", "/repos/o/r/", 1)
	switch {
	case p == "/repos/o/r":
		name := f.name
		if name == "" {
			name = "o/r"
		}
		fmt.Fprintf(w, `{"visibility":"public","full_name":%q}`, name)
	case p == "/repos/o/sub":
		fmt.Fprint(w, `{"visibility":"public","full_name":"o/sub"}`)
	case p == "/repos/o/r/tags":
		fmt.Fprint(w, `[{"name":"v2"},{"name":"v2.1.1"},{"name":"v2.1"},{"name":"v1"}]`)
	case strings.HasPrefix(p, "/repos/o/r/commits/"):
		sha := strings.Repeat("a", 40)
		if strings.HasSuffix(p, "/v1") {
			sha = strings.Repeat("b", 40)
		}
		fmt.Fprintf(w, `{"sha":%q}`, sha)
	case strings.HasPrefix(p, "/repos/o/r/zipball/"):
		f.archives++
		w.Header().Set("Content-Type", "application/zip")
		w.Write(f.zip)
	case strings.HasPrefix(p, "/repos/o/r/tarball/"):
		f.archives++
		w.Header().Set("Content-Type", "application/gzip")
		w.Write(f.tar)
	default:
		http.Error(w, "unexpected fixture request", 400)
	}
}

func run(t *testing.T, cwd, endpoint string, args ...string) (string, string, error) {
	t.Helper()
	cmd := exec.Command(binary, args...)
	cmd.Dir = cwd
	config := filepath.Join(cwd, "isolated-gh")
	if err := os.MkdirAll(config, 0o700); err != nil {
		t.Fatal(err)
	}
	token := "public-secret"
	for _, arg := range args {
		if arg == "tenant.ghe.com" {
			token = "tenant-secret"
		}
	}
	cmd.Env = append(os.Environ(),
		"GH_CONFIG_DIR="+config, "GH_TOKEN="+token, "GITHUB_TOKEN=",
		"GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN=public-secret",
		"ARCHIVE_CACHE_TEST_ENDPOINT="+endpoint,
		"ARCHIVE_CACHE_TEST_CONFIG_ROOT="+cwd,
		"ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE=", "SHELL=/bin/bash", "GH_DEBUG=")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err := cmd.Run()
	return out.String(), stderr.String(), err
}

func write(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func snapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			files[rel] = string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestCompiledDownloadReuseAndCleanup(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	f := &fixture{zip: fixtureArchive(t, "zip"), tar: fixtureArchive(t, "tar.gz")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	base := []string{"--hostname", "github.com", "--cache-dir", cache, "--format", "both"}
	for range 2 {
		out, stderr, err := run(t, root, server.URL, append(base, "download", "o/r")...)
		if err != nil {
			t.Fatalf("download: %v\n%s\n%s", err, out, stderr)
		}
	}
	f.mu.Lock()
	count := f.archives
	f.mu.Unlock()
	if count != 4 {
		t.Fatalf("aliases/repeat downloads fetched %d archives, want 4", count)
	}
	missing := filepath.Join(cache, "o_r", strings.Repeat("a", 40)+".tar.gz")
	if err := os.Remove(missing); err != nil {
		t.Fatal(err)
	}
	if out, stderr, err := run(t, root, server.URL, append(base, "download", "o/r")...); err != nil {
		t.Fatalf("missing format: %v %s %s", err, out, stderr)
	}
	f.mu.Lock()
	count = f.archives
	f.mu.Unlock()
	if count != 5 {
		t.Fatalf("missing format downloaded %d archives", count)
	}
	write(t, filepath.Join(cache, "o_r", "unidentified.data"), []byte("preserve"))
	before := snapshot(t, cache)
	cleanup := append(base, "cleanup", "o/r", "--latest-major-versions", "1", "--latest-versions-per-major", "1")
	out, stderr, err := run(t, root, server.URL, append(cleanup, "--dry-run")...)
	if err != nil || !strings.Contains(stderr, "warning: preserving unidentified file") {
		t.Fatalf("cleanup preview: %v %s %s", err, out, stderr)
	}
	if !reflect.DeepEqual(before, snapshot(t, cache)) {
		t.Fatal("dry-run changed cache")
	}
	out, stderr, err = run(t, root, server.URL, cleanup...)
	if err != nil || !strings.Contains(out, "removed=2") || !strings.Contains(stderr, "unidentified.data") {
		t.Fatalf("cleanup: %v %s %s", err, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(cache, "o_r", strings.Repeat("a", 40)+".zip")); err != nil {
		t.Fatal("latest archive removed")
	}
}

func lockData(host string) []byte {
	return []byte(fmt.Sprintf("version: v0.0.3\nworkflows:\n  .github/workflows/ci.yml:\n    - o/r@v1\ndependencies:\n  o/r@v1:\n    hostname: %s\n    ref: v1\n    commit: sha1-%s\n    owner_id: 1\n    repo_id: 2\n", host, strings.Repeat("b", 40)))
}

func TestPinnedOfflineReuseAndStrictHosts(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	lock := filepath.Join(root, "actions.lock")
	write(t, lock, lockData("tenant.ghe.com"))
	f := &fixture{zip: fixtureArchive(t, "zip"), tar: fixtureArchive(t, "tar.gz")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	args := []string{"from-lockfile", lock, "--cache-dir", cache, "--hostname", "tenant.ghe.com", "--format", "zip"}
	if out, stderr, err := run(t, root, server.URL, args...); err != nil {
		t.Fatalf("import: %v %s %s", err, out, stderr)
	}
	f.mu.Lock()
	before := len(f.requests)
	f.offline = true
	f.mu.Unlock()
	if out, stderr, err := run(t, root, server.URL, args...); err != nil || !strings.Contains(out, "cached=1") {
		t.Fatalf("offline cached import: %v %s %s", err, out, stderr)
	}
	f.mu.Lock()
	if len(f.requests) != before {
		t.Error("cached pinned import made HTTP request")
	}
	f.offline, f.missing = false, true
	f.requests = nil
	f.mu.Unlock()
	args = []string{"from-lockfile", lock, "--cache-dir", filepath.Join(root, "fresh"), "--hostname", "tenant.ghe.com", "--format", "zip"}
	if _, _, err := run(t, root, server.URL, args...); err == nil {
		t.Fatal("missing tenant pin succeeded")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 1 || !strings.HasPrefix(f.requests[0], "api.tenant.ghe.com/") {
		t.Fatalf("locked pin fell back: %v", f.requests)
	}
}

func TestCanonicalRepositoryCasing(t *testing.T) {
	for _, command := range []string{"download", "from-lockfile"} {
		t.Run(command, func(t *testing.T) {
			root := t.TempDir()
			cache := filepath.Join(root, "cache")
			lock := filepath.Join(root, "actions.lock")
			write(t, lock, lockData("github.com"))
			f := &fixture{name: "O/R", zip: fixtureArchive(t, "zip"), tar: fixtureArchive(t, "tar.gz")}
			server := httptest.NewServer(http.HandlerFunc(f.serve))
			defer server.Close()
			base := []string{"--hostname", "github.com", "--cache-dir", cache, "--format", "both"}
			input := lock
			if command == "download" {
				input = "o/r"
			}
			out, stderr, err := run(t, root, server.URL, append(base, command, input)...)
			if err != nil {
				t.Fatalf("preload: %v %s %s", err, out, stderr)
			}
			dirs, err := os.ReadDir(cache)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, dir := range dirs {
				if dir.IsDir() {
					if dir.Name() != "O_R" {
						t.Fatalf("wrong directory casing %q", dir.Name())
					}
					found = true
				}
			}
			if !found {
				t.Fatal("canonical repository directory missing")
			}
			f.mu.Lock()
			before := len(f.requests)
			f.offline = true
			f.mu.Unlock()
			// Remove the index to exercise local discovery of a pre-existing cache.
			if err := os.Remove(filepath.Join(cache, ".gh-actions-archive-cache.json")); err != nil {
				t.Fatal(err)
			}
			out, stderr, err = run(t, root, server.URL, append(base, "from-lockfile", lock)...)
			if err != nil || !strings.Contains(out, "cached=2") {
				t.Fatalf("offline import: %v %s %s", err, out, stderr)
			}
			f.mu.Lock()
			after := len(f.requests)
			f.offline = false
			f.mu.Unlock()
			if after != before {
				t.Fatal("cached canonical import made network requests")
			}
			out, stderr, err = run(t, root, server.URL, append(base, "cleanup", "o/r", "--latest-major-versions", "1")...)
			retained := "retained=2"
			if command == "download" {
				retained = "retained=4"
			}
			if err != nil || !strings.Contains(out, retained) {
				t.Fatalf("canonical cleanup: %v %s %s", err, out, stderr)
			}
		})
	}
}

func TestTenantFallbackCredentialIsolation(t *testing.T) {
	root := t.TempDir()
	f := &fixture{missing: true, zip: fixtureArchive(t, "zip")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	out, stderr, err := run(t, root, server.URL, "download", "o/r@v1", "--hostname", "tenant.ghe.com",
		"--cache-dir", filepath.Join(root, "cache"), "--format", "zip")
	if err != nil {
		t.Fatalf("fallback: %v %s %s", err, out, stderr)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) < 4 || !strings.HasPrefix(f.requests[0], "api.tenant.ghe.com/") ||
		!strings.HasPrefix(f.requests[1], "api.github.com/") {
		t.Fatalf("tenant-first order violated: %v", f.requests)
	}
}

func TestCompiledConfigurationAndInvalidArguments(t *testing.T) {
	root := t.TempDir()
	runner, cache := filepath.Join(root, "runner"), filepath.Join(root, "cache")
	listener := "Runner.Listener"
	if runtime.GOOS == "windows" {
		listener += ".exe"
	}
	write(t, filepath.Join(runner, "bin", listener), nil)
	write(t, filepath.Join(runner, ".env"), []byte("OTHER=keep\r\n"))
	args := []string{"configure", "--scope", "runner", "--runner-path", runner, "--cache-dir", cache}
	before := snapshot(t, runner)
	if out, stderr, err := run(t, root, "", append(args, "--dry-run")...); err != nil {
		t.Fatalf("config preview: %v %s %s", err, out, stderr)
	}
	if !reflect.DeepEqual(before, snapshot(t, runner)) {
		t.Fatal("configuration dry-run changed files")
	}
	for range 2 {
		if out, stderr, err := run(t, root, "", args...); err != nil {
			t.Fatalf("configure: %v %s %s", err, out, stderr)
		}
	}
	b, err := os.ReadFile(filepath.Join(runner, ".env"))
	if err != nil || bytes.Count(b, []byte("ACTIONS_RUNNER_ACTION_ARCHIVE_CACHE=")) != 1 || !bytes.Contains(b, []byte("OTHER=keep\r\n")) {
		t.Fatalf("configuration preservation failed: %s %v", b, err)
	}
	for _, scope := range []string{"user", "system"} {
		if out, stderr, err := run(t, root, "", "configure", "--scope", scope, "--cache-dir", cache); err != nil {
			t.Fatalf("isolated %s backend: %v %s %s", scope, err, out, stderr)
		}
	}
	_, stderr, err := run(t, root, "", "cleanup", "o/r")
	if err == nil || !strings.Contains(stderr, "positive retention limit") {
		t.Fatalf("invalid cleanup not rejected: %v %s", err, stderr)
	}
}

func TestMalformedLockfileDoesNotDelete(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	p := filepath.Join(cache, "o_r", strings.Repeat("b", 40)+".zip")
	write(t, p, fixtureArchive(t, "zip"))
	lock := filepath.Join(root, "broken.lock")
	write(t, lock, []byte("version: [broken"))
	before := snapshot(t, cache)
	_, _, err := run(t, root, "", "cleanup", "o/r", "--cache-dir", cache, "--lockfile", lock,
		"--latest-major-versions", "1", "--hostname", "github.com")
	if err == nil || !reflect.DeepEqual(before, snapshot(t, cache)) {
		t.Fatal("malformed protection lockfile allowed mutation")
	}
}

func TestFixtureMetadataPersists(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	f := &fixture{zip: fixtureArchive(t, "zip")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	if out, stderr, err := run(t, root, server.URL, "download", "o/r@feature/test", "--hostname", "github.com", "--cache-dir", cache, "--format", "zip"); err != nil {
		t.Fatalf("exact branch: %v %s %s", err, out, stderr)
	}

	b, err := os.ReadFile(filepath.Join(cache, ".gh-actions-archive-cache.json"))
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Entries []struct {
			Exact bool
			Ref   string
		}
	}
	if err := json.Unmarshal(b, &data); err != nil || len(data.Entries) != 1 || !data.Entries[0].Exact || data.Entries[0].Ref != "feature/test" {
		t.Fatalf("metadata did not persist exact ref: %s %v", b, err)
	}
}

func TestTransitiveLockfileAndSuppliedPinProtection(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	lock := filepath.Join(root, "transitive.lock")
	sha := strings.Repeat("b", 40)
	data := string(lockData("github.com"))
	data += "    uses:\n      - o/sub@v2\n  o/sub@v2:\n    hostname: github.com\n    ref: v2\n    commit: sha1-" + sha + "\n    owner_id: 1\n    repo_id: 3\n"
	write(t, lock, []byte(data))
	f := &fixture{zip: fixtureArchive(t, "zip")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	base := []string{"--hostname", "tenant.ghe.com", "--cache-dir", cache, "--format", "zip"}
	out, stderr, err := run(t, root, server.URL, append(base, "from-lockfile", lock)...)
	if err != nil || !strings.Contains(out, "downloaded=2") {
		t.Fatalf("transitive import failed: %v %s %s", err, out, stderr)
	}
	// Imported github.com identity must stay authoritative during cleanup too.
	publicBase := []string{"--hostname", "github.com", "--cache-dir", cache, "--format", "zip"}
	out, stderr, err = run(t, root, server.URL, append(publicBase, "cleanup", "o/r", "--latest-major-versions", "1", "--lockfile", lock)...)
	if err != nil {
		t.Fatalf("pin cleanup failed: %v %s %s", err, out, stderr)
	}
	if _, err := os.Stat(filepath.Join(cache, "o_r", sha+".zip")); err != nil {
		t.Fatal("lockfile pin removed")
	}
}

func TestFailedDownloadAndCleanupPlanning(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(root, "cache")
	f := &fixture{zip: []byte("invalid zip")}
	server := httptest.NewServer(http.HandlerFunc(f.serve))
	defer server.Close()
	out, stderr, err := run(t, root, server.URL, "download", "o/r@v1", "--hostname", "github.com", "--cache-dir", cache, "--format", "zip")
	if err == nil || !strings.Contains(stderr, "invalid downloaded archive") {
		t.Fatalf("corrupt download accepted: %v %s %s", err, out, stderr)
	}
	p := filepath.Join(cache, "o_r", strings.Repeat("b", 40)+".zip")
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("failed download published final archive")
	}
	write(t, p, fixtureArchive(t, "zip"))
	before := snapshot(t, cache)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "unavailable", 403) }))
	defer bad.Close()
	_, _, err = run(t, root, bad.URL, "cleanup", "o/r", "--hostname", "github.com", "--cache-dir", cache, "--format", "zip", "--latest-major-versions", "1")
	if err == nil || !reflect.DeepEqual(before, snapshot(t, cache)) {
		t.Fatal("failed cleanup planning mutated cache")
	}
}
