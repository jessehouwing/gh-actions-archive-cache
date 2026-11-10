package app

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func archiveBytes(t *testing.T, format string) []byte {
	t.Helper()
	var b bytes.Buffer
	if format == "zip" {
		w := zip.NewWriter(&b)
		if _, err := w.Create("repo-sha/"); err != nil {
			t.Fatal(err)
		}
		f, err := w.Create("repo-sha/action.yml")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write([]byte("name: fixture\n")); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		g := gzip.NewWriter(&b)
		w := tar.NewWriter(g)
		if err := w.WriteHeader(&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader,
			PAXRecords: map[string]string{"comment": "git archive fixture"}}); err != nil {
			t.Fatal(err)
		}
		if err := w.WriteHeader(&tar.Header{Name: "repo-sha/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
			t.Fatal(err)
		}
		data := "name: fixture\n"
		if err := w.WriteHeader(&tar.Header{Name: "repo-sha/action.yml", Size: int64(len(data)), Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(w, data); err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(w.Close(), g.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return b.Bytes()
}

func writeTestFile(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveValidation(t *testing.T) {
	for _, format := range []string{"zip", "tar.gz"} {
		t.Run(format, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "archive")
			data := archiveBytes(t, format)
			writeTestFile(t, p, data)
			if err := validateArchive(p, format); err != nil {
				t.Fatal(err)
			}

			writeTestFile(t, p, data[:len(data)/2])
			if err := validateArchive(p, format); err == nil {
				t.Fatal("truncated archive accepted")
			}
		})
	}
}

func TestArchiveTraversalAndMultipleRootsRejected(t *testing.T) {
	for _, names := range [][]string{{"../outside", "root/file"}, {"a/file", "b/file"}, {"/absolute/file", "root/file"}} {
		var b bytes.Buffer
		z := zip.NewWriter(&b)
		for _, name := range names {
			w, err := z.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.Write([]byte("data")); err != nil {
				t.Fatal(err)
			}
		}
		if err := z.Close(); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(t.TempDir(), "bad.zip")
		writeTestFile(t, p, b.Bytes())
		if err := validateArchive(p, "zip"); err == nil {
			t.Fatalf("unsafe archive accepted: %v", names)
		}
	}
}

func TestDownloadReuseAndFormatIndependence(t *testing.T) {
	c, release, err := openCache(filepath.Join(t.TempDir(), "cache"), false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	sha := strings.Repeat("a", 40)
	es := []entry{
		{Host: "github.com", Repo: "o/r", SHA: sha, Ref: "v1", Format: "zip"},
		{Host: "github.com", Repo: "o/r", SHA: sha, Ref: "v1.0", Format: "zip"},
		{Host: "github.com", Repo: "o/r", SHA: sha, Ref: "v1", Format: "tar.gz"},
	}
	g, _ := newGitHub("github.com")
	g.names["github.com/o/r"] = "o/r"
	calls := 0
	g.clients[g.home] = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		format := "tar.gz"
		if strings.Contains(r.URL.Path, "/zipball/") {
			format = "zip"
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(archiveBytes(t, format))), Header: http.Header{}}, nil
	})}
	for range 2 {
		if err := c.download(context.Background(), g, es, false, io.Discard, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 2 || len(c.data.Entries) != 3 {
		t.Fatalf("calls=%d records=%d", calls, len(c.data.Entries))
	}
	p, _ := c.destination(es[2])
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	if err := c.download(context.Background(), g, es, false, io.Discard, io.Discard); err != nil || calls != 3 {
		t.Fatalf("format-specific skip failed: %v calls=%d", err, calls)
	}
}

func TestInvalidDownloadLeavesNoArchiveOrMetadataEntry(t *testing.T) {
	c, release, err := openCache(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	e := entry{Host: "github.com", Repo: "o/r", SHA: strings.Repeat("a", 40), Ref: "v1", Format: "zip"}
	g, _ := newGitHub("github.com")
	g.names["github.com/o/r"] = "o/r"
	g.clients[g.home] = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return response(200, "not an archive"), nil
	})}
	if err := c.download(context.Background(), g, []entry{e, e}, false, io.Discard, io.Discard); err == nil {
		t.Fatal("invalid download succeeded")
	}
	if len(c.data.Entries) != 0 {
		t.Fatal("failed download recorded")
	}
	p, _ := c.destination(e)
	if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("failed download left final file")
	}
	files, err := os.ReadDir(filepath.Dir(p))
	if err != nil || len(files) != 0 {
		t.Fatalf("temporary files remain: %v %v", files, err)
	}
}

func TestCacheLockAndReadOnlyDryRun(t *testing.T) {
	root := filepath.Join(t.TempDir(), "absent")
	c, release, err := openCache(root, true)
	if err != nil || c.root != root {
		t.Fatalf("dry open: %v", err)
	}
	release()
	if _, err := os.Stat(root); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("dry run created cache")
	}
	_, release, err = openCache(root, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, dry := range []bool{false, true} {
		if _, _, err := openCache(root, dry); err == nil {
			t.Fatal("concurrent lock allowed")
		}
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestCacheRefusesSymlinkAndCorruptDestination(t *testing.T) {
	c := &cache{root: t.TempDir()}
	e := entry{Repo: "o/r", SHA: strings.Repeat("a", 40), Format: "zip"}
	p, _ := c.destination(e)
	writeTestFile(t, p, []byte("bad"))
	if _, err := c.present(e); err == nil {
		t.Fatal("corrupt cached file accepted")
	}
	if err := os.Remove(p); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "outside")
	writeTestFile(t, target, archiveBytes(t, "zip"))
	if err := os.Symlink(target, p); err != nil {
		t.Skipf("symlink privilege unavailable: %v", err)
	}
	if _, err := c.present(e); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestCanonicalRepositoryDirectory(t *testing.T) {
	c := &cache{root: t.TempDir()}
	e := entry{Repo: "azure/login", Name: "Azure/login", SHA: strings.Repeat("a", 40), Format: "zip"}
	p, err := c.destination(e)
	if err != nil || filepath.Base(filepath.Dir(p)) != "Azure_login" {
		t.Fatalf("canonical path: %q %v", p, err)
	}
	writeTestFile(t, p, archiveBytes(t, "zip"))
	e.Name = ""
	if cached, err := c.present(e); err != nil || !cached {
		t.Fatalf("unindexed canonical archive not found: %t %v", cached, err)
	}
	e.Name = "../outside"
	if _, err := c.destination(e); err == nil {
		t.Fatal("invalid canonical directory accepted")
	}
	e.Name = "Different/repository"
	if _, err := c.destination(e); err == nil {
		t.Fatal("mismatched canonical repository accepted")
	}
}

func TestCleanupProtectionAndWarnings(t *testing.T) {
	c, release, err := openCache(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	latest, old, pinned, exact := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	g, _ := newGitHub("github.com")
	g.clients[g.home] = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/tags"):
			return response(200, `[{"name":"v2"},{"name":"v2.0"},{"name":"v1"}]`), nil
		case strings.Contains(r.URL.Path, "/commits/v1"):
			return response(200, `{"sha":"`+old+`"}`), nil
		case strings.Contains(r.URL.Path, "/commits/"):
			return response(200, `{"sha":"`+latest+`"}`), nil
		default:
			return response(200, `{"visibility":"public","full_name":"o/r"}`), nil
		}
	})}
	for _, sha := range []string{latest, old, pinned, exact} {
		e := entry{Host: "github.com", Repo: "o/r", SHA: sha, Format: "zip", Ref: "v1", Pinned: sha == pinned, Exact: sha == exact}
		p, _ := c.destination(e)
		writeTestFile(t, p, archiveBytes(t, "zip"))
		c.record(e)
	}
	unknown := filepath.Join(c.root, "o_r", "unknown.txt")
	writeTestFile(t, unknown, []byte("keep"))
	for _, dry := range []bool{true, false} {
		var out, stderr bytes.Buffer
		if err := c.cleanup(context.Background(), g, "o/r", 1, 1, []string{"zip"}, nil, dry, &out, &stderr); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(stderr.String(), "warning: preserving unidentified file") || !strings.Contains(stderr.String(), "unknown.txt") {
			t.Fatalf("missing warning: %s", stderr.String())
		}
		if _, err := os.Stat(unknown); err != nil {
			t.Fatal("unknown file deleted")
		}
		for _, sha := range []string{latest, pinned, exact} {
			p, _ := c.destination(entry{Repo: "o/r", SHA: sha, Format: "zip"})
			if _, err := os.Stat(p); err != nil {
				t.Fatalf("protected file deleted: %s", sha)
			}
		}
		p, _ := c.destination(entry{Repo: "o/r", SHA: old, Format: "zip"})
		_, err := os.Stat(p)
		if dry && err != nil || !dry && !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleanup dry=%t file status: %v", dry, err)
		}
	}
}
