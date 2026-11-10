package app

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
)

const indexName = ".gh-actions-archive-cache.json"
const lockName = ".gh-actions-archive-cache.lock"
const maxArchiveSize = int64(2 << 30)

type index struct {
	Version int     `json:"version"`
	Entries []entry `json:"entries"`
}

type cache struct {
	root       string
	data       index
	jsonOutput bool
}

type downloadResult struct {
	Host       string `json:"host"`
	Repository string `json:"repository"`
	Ref        string `json:"ref"`
	SHA        string `json:"sha"`
	Format     string `json:"format"`
	Path       string `json:"path"`
	Status     string `json:"status"`
	Error      string `json:"error,omitempty"`
}

type downloadReport struct {
	CacheDirectory string           `json:"cache_directory"`
	DryRun         bool             `json:"dry_run"`
	Results        []downloadResult `json:"results"`
	Downloaded     int              `json:"downloaded"`
	Cached         int              `json:"cached"`
	Planned        int              `json:"planned"`
	Failed         int              `json:"failed"`
	Errors         []string         `json:"errors"`
}

func safeDirectory(dir string, create bool) error {
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) && create {
		parent := filepath.Dir(dir)
		if parent != dir {
			if err := safeDirectory(parent, create); err != nil {
				return err
			}
		}
		if err := os.Mkdir(dir, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
			return err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("unsafe cache directory %q: not a real directory", dir)
	}
	return nil
}

func openCache(root string, dry bool) (*cache, func() error, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	c := &cache{root: absolute, data: index{Version: 1}}
	if err := safeDirectory(absolute, !dry); err != nil {
		if dry && errors.Is(err, os.ErrNotExist) {
			return c, func() error { return nil }, nil
		}
		return nil, nil, err
	}
	lockPath := filepath.Join(absolute, lockName)
	release := func() error { return nil }
	if dry {
		if _, err := os.Lstat(lockPath); err == nil {
			return nil, nil, errors.New("cache is locked by another operation; retry when it finishes")
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, nil, err
		}
	} else {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("lock cache (if a prior process crashed, inspect and remove %q manually): %w", lockPath, err)
		}
		if err := f.Close(); err != nil {
			return nil, nil, errors.Join(err, os.Remove(lockPath))
		}
		release = func() error { return os.Remove(lockPath) }
	}
	if err := c.load(); err != nil {
		return nil, nil, errors.Join(err, release())
	}
	return c, release, nil
}

func (c *cache) destination(e entry) (string, error) {
	_, repoName, _ := strings.Cut(e.Repo, "/")
	if !repoPattern.MatchString(e.Repo) || repoName == "." || repoName == ".." || !shaPattern.MatchString(e.SHA) ||
		(e.Format != "zip" && e.Format != "tar.gz") {
		return "", fmt.Errorf("invalid cache entry for %q", e.Repo)
	}
	dir, err := c.repositoryDirectory(e.Repo, e.Name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, e.SHA+"."+e.Format), nil
}

func (c *cache) repositoryDirectory(repo, name string) (string, error) {
	if name == "" {
		for _, known := range c.data.Entries {
			if known.Repo == repo && known.Name != "" {
				name = known.Name
				break
			}
		}
	}
	if name != "" {
		if !repoPattern.MatchString(name) || !strings.EqualFold(name, repo) {
			return "", fmt.Errorf("invalid canonical repository name %q for %s", name, repo)
		}
		return filepath.Join(c.root, strings.ReplaceAll(name, "/", "_")), nil
	}
	files, err := os.ReadDir(c.root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	wanted := strings.ReplaceAll(repo, "/", "_")
	found := ""
	for _, file := range files {
		if strings.EqualFold(file.Name(), wanted) {
			if found != "" {
				return "", fmt.Errorf("ambiguous cache directory casing for %s", repo)
			}
			found = file.Name()
		}
	}
	if found == "" {
		found = wanted
	}
	return filepath.Join(c.root, found), nil
}

func (c *cache) load() error {
	p := filepath.Join(c.root, indexName)
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("metadata %q is not a regular file", p)
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 16<<20))
	d.DisallowUnknownFields()
	if err := d.Decode(&c.data); err != nil {
		return fmt.Errorf("read metadata: %w", err)
	}
	if c.data.Version != 1 {
		return fmt.Errorf("unsupported cache metadata version %d", c.data.Version)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing cache metadata")
	}
	hosts := map[string]string{}
	for _, e := range c.data.Entries {
		if _, err := c.destination(e); err != nil {
			return err
		}
		if e.Host != "github.com" && !tenantPattern.MatchString(e.Host) {
			return fmt.Errorf("invalid metadata host %q", e.Host)
		}
		if prev := hosts[e.Repo]; prev != "" && prev != e.Host {
			return fmt.Errorf("cache repository %s has conflicting hosts", e.Repo)
		}
		hosts[e.Repo] = e.Host
	}
	return nil
}

func (c *cache) checkHosts(entries []entry) error {
	hosts := map[string]string{}
	for _, list := range [][]entry{c.data.Entries, entries} {
		for _, e := range list {
			if prev := hosts[e.Repo]; prev != "" && prev != e.Host {
				return fmt.Errorf("cache cannot mix %s repositories from %s and %s", e.Repo, prev, e.Host)
			}
			hosts[e.Repo] = e.Host
		}
	}
	return nil
}

func (c *cache) record(e entry) {
	for i, old := range c.data.Entries {
		if old == e {
			return
		}
		if old.key() == e.key() && old.Host == e.Host && old.Ref == e.Ref {
			e.Exact = e.Exact || old.Exact
			e.Pinned = e.Pinned || old.Pinned
			c.data.Entries[i] = e
			return
		}
	}
	c.data.Entries = append(c.data.Entries, e)
}

func (c *cache) save() error {
	slices.SortFunc(c.data.Entries, func(a, b entry) int {
		return strings.Compare(a.key()+a.Ref, b.key()+b.Ref)
	})
	b, err := json.MarshalIndent(c.data, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(c.root, indexName), append(b, '\n'), 0o600)
}

func atomicWrite(p string, data []byte, mode os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(p), ".archive-cache-write-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer func() {
		if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	writeErr := f.Chmod(mode)
	if writeErr == nil {
		_, writeErr = f.Write(data)
	}
	if writeErr == nil {
		writeErr = f.Sync()
	}
	if err := errors.Join(writeErr, f.Close()); err != nil {
		return err
	}
	return replaceFile(temp, p)
}

func validateArchive(p, format string) error {
	root := ""
	count := 0
	check := func(name string) error {
		clean := path.Clean(strings.TrimSuffix(name, "/"))
		if strings.ContainsAny(name, "\\\x00") || path.IsAbs(name) || clean == "." ||
			clean == ".." || strings.HasPrefix(clean, "../") {
			return fmt.Errorf("unsafe archive entry %q", name)
		}
		first, _, _ := strings.Cut(clean, "/")
		if root != "" && first != root {
			return errors.New("archive must contain one enclosing repository directory")
		}
		root = first
		count++
		return nil
	}
	if format == "zip" {
		z, err := zip.OpenReader(p)
		if err != nil {
			return err
		}
		defer z.Close()
		var expanded int64
		for _, f := range z.File {
			if err := check(f.Name); err != nil {
				return err
			}
			r, err := f.Open()
			if err != nil {
				return err
			}
			n, readErr := io.Copy(io.Discard, io.LimitReader(r, maxArchiveSize-expanded+1))
			closeErr := r.Close()
			expanded += n
			if expanded > maxArchiveSize {
				return errors.New("archive exceeds validation size limit")
			}
			if err := errors.Join(readErr, closeErr); err != nil {
				return err
			}
		}
	} else {
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		z, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer z.Close()
		limited := &io.LimitedReader{R: z, N: maxArchiveSize + 1}
		t := tar.NewReader(limited)
		for {
			h, err := t.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if h.Typeflag == tar.TypeXGlobalHeader {
				continue
			}
			if err := check(h.Name); err != nil {
				return err
			}
		}
		if _, err := io.Copy(io.Discard, limited); err != nil {
			return err
		}
		if limited.N == 0 {
			return errors.New("archive exceeds validation size limit")
		}
	}
	if count < 2 {
		return errors.New("archive lacks enclosing directory and repository contents")
	}
	return nil
}

func (c *cache) present(e entry) (bool, error) {
	p, err := c.destination(e)
	if err != nil {
		return false, err
	}
	if err := safeDirectory(c.root, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if err := safeDirectory(filepath.Dir(p), false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("cache destination %q is not a regular file", p)
	}
	if err := validateArchive(p, e.Format); err != nil {
		return false, fmt.Errorf("cached archive %q is invalid; inspect and remove it before retrying: %w", p, err)
	}
	return true, nil
}

func (c *cache) fetch(ctx context.Context, g *github, e entry) (err error) {
	p, err := c.destination(e)
	if err != nil {
		return err
	}
	if err := safeDirectory(filepath.Dir(p), true); err != nil {
		return err
	}
	endpoint := "tarball"
	if e.Format == "zip" {
		endpoint = "zipball"
	}
	r, err := g.get(ctx, e.Host, "repos/"+e.Repo+"/"+endpoint+"/"+e.SHA)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	f, err := os.CreateTemp(filepath.Dir(p), ".archive-download-*")
	if err != nil {
		return err
	}
	temp := f.Name()
	defer func() {
		if removeErr := os.Remove(temp); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			err = errors.Join(err, removeErr)
		}
	}()
	n, copyErr := io.Copy(f, io.LimitReader(r.Body, maxArchiveSize+1))
	if err := errors.Join(copyErr, f.Sync(), f.Close()); err != nil {
		return err
	}
	if n > maxArchiveSize {
		return errors.New("download exceeds 2 GiB archive limit")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validateArchive(temp, e.Format); err != nil {
		return fmt.Errorf("invalid downloaded archive: %w", err)
	}
	if err := os.Chmod(temp, 0o644); err != nil {
		return err
	}
	if _, err := os.Lstat(p); err == nil {
		return fmt.Errorf("destination appeared during download: %q; refusing to overwrite", p)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(temp, p)
}

func (c *cache) download(ctx context.Context, g *github, entries []entry, dry bool, out, stderr io.Writer) error {
	if err := c.checkHosts(entries); err != nil {
		return err
	}
	seen := map[string]bool{}
	success := map[string]entry{}
	cached, downloaded, failed := 0, 0, 0
	planned := 0
	unique := map[string]bool{}
	for _, e := range entries {
		unique[e.key()] = true
	}
	total := len(unique)
	report := downloadReport{CacheDirectory: c.root, DryRun: dry, Results: []downloadResult{}, Errors: []string{}}
	if !c.jsonOutput {
		fmt.Fprintf(out, "Cache: %s\n", c.root)
		fmt.Fprintf(stderr, "Processing %d archives sequentially (including local validation)...\n", total)
	}
	var errs []error
	for _, e := range entries {
		if seen[e.key()] {
			if previous := success[e.key()]; !dry && previous.Repo != "" {
				e.Name = previous.Name
				c.record(e)
			}
			continue
		}
		seen[e.key()] = true
		label := e.Ref
		if label == "" || shaPattern.MatchString(label) {
			label = e.SHA[:min(12, len(e.SHA))]
		}
		status := "failed"
		p, err := c.destination(e)
		var exists bool
		if err == nil {
			exists, err = c.present(e)
		}
		if err == nil && !exists && e.Name == "" {
			e.Name, err = g.canonicalName(ctx, e.Host, e.Repo)
			if err == nil {
				p, err = c.destination(e)
				if err == nil {
					exists, err = c.present(e)
				}
			}
		}
		if err == nil {
			if e.Name == "" {
				e.Name = strings.Replace(filepath.Base(filepath.Dir(p)), "_", "/", 1)
			}
			switch {
			case exists:
				cached++
				status = "cached"
			case dry:
				planned++
				status = "planned"
			default:
				if !c.jsonOutput {
					fmt.Fprintf(stderr, "[%d/%d] Downloading %s@%s (%s) from %s...\n", len(seen), total, e.Repo, label, e.Format, e.Host)
				}
				err = c.fetch(ctx, g, e)
				if err == nil {
					downloaded++
					status = "downloaded"
				}
			}
		}
		item := downloadResult{Host: e.Host, Repository: e.Repo, Ref: e.Ref, SHA: e.SHA, Format: e.Format, Path: p, Status: status}
		if err != nil {
			item.Error = err.Error()
		}
		report.Results = append(report.Results, item)
		if !c.jsonOutput {
			displayStatus := map[string]string{"cached": "Cached", "downloaded": "Downloaded", "planned": "Would download", "failed": "Failed"}[status]
			fmt.Fprintf(out, "[%d/%d] %-14s %s@%s (%s, %s)\n", len(seen), total, displayStatus, e.Repo, label, e.Format, e.Host)
		}
		if err != nil {
			failed++
			errs = append(errs, err)
			fmt.Fprintln(stderr, "error:", err)
			continue
		}
		if !dry {
			c.record(e)
		}
		success[e.key()] = e
	}
	if !dry {
		errs = append(errs, c.save())
	}
	if c.jsonOutput {
		report.Downloaded, report.Cached, report.Planned, report.Failed = downloaded, cached, planned, failed
		for _, err := range errs {
			if err != nil {
				report.Errors = append(report.Errors, err.Error())
			}
		}
		errs = append(errs, json.NewEncoder(out).Encode(report))
	} else {
		fmt.Fprintf(out, "\nDone: %d downloaded, %d cached, %d planned, %d failed", downloaded, cached, planned, failed)
		if dry {
			fmt.Fprint(out, " (dry run)")
		}
		fmt.Fprintln(out)
	}
	return errors.Join(errs...)
}
