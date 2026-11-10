package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type candidate struct {
	path string
	info os.FileInfo
}

func (c *cache) cleanup(ctx context.Context, g *github, repo string, majors, perMajor int, formats []string, pins []entry, dry bool, out, stderr io.Writer) error {
	all, err := g.resolve(ctx, repo, 0, 0, formats)
	if err != nil {
		return err
	}
	if err := c.checkHosts(append(all, pins...)); err != nil {
		return err
	}
	var tags []string
	for _, e := range all {
		tags = append(tags, e.Ref)
	}
	keepTags, err := selectTags(tags, majors, perMajor)
	if err != nil {
		return err
	}
	keepTag := map[string]bool{}
	currentTag := map[string]bool{}
	for _, e := range all {
		currentTag[e.Ref] = true
	}
	for _, t := range keepTags {
		keepTag[t] = true
	}
	known := map[string]bool{}
	keep := map[string]bool{}
	for _, e := range all {
		known[e.key()] = true
		if keepTag[e.Ref] {
			keep[e.key()] = true
		}
	}
	for _, e := range c.data.Entries {
		if e.Repo != repo {
			continue
		}
		if _, stable := parseVersion(e.Ref); stable && currentTag[e.Ref] {
			known[e.key()] = true
		}
		if e.Pinned || e.Exact {
			keep[e.key()] = true
		}
	}
	for _, e := range pins {
		if e.Repo == repo {
			keep[e.key()] = true
		}
	}
	dir, err := c.repositoryDirectory(repo, all[0].Name)
	if err != nil {
		return err
	}
	if err := safeDirectory(c.root, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(out, "removed=0 retained=0 unidentified=0 bytes=0")
			return nil
		}
		return err
	}
	if err := safeDirectory(dir, false); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Fprintln(out, "removed=0 retained=0 unidentified=0 bytes=0")
			return nil
		}
		return err
	}
	files, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var planned []candidate
	retained, unidentified := 0, 0
	var bytes int64
	for _, f := range files {
		p := filepath.Join(dir, f.Name())
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		format := ""
		for _, ext := range []string{"tar.gz", "zip"} {
			if strings.HasSuffix(f.Name(), "."+ext) {
				format = ext
				break
			}
		}
		sha := strings.TrimSuffix(f.Name(), "."+format)
		e := entry{Repo: repo, SHA: sha, Format: format}
		if info.Mode().IsRegular() && shaPattern.MatchString(sha) && format != "" && !slices.Contains(formats, format) {
			retained++
			continue
		}
		if !info.Mode().IsRegular() || !shaPattern.MatchString(sha) || format == "" || (!known[e.key()] && !keep[e.key()]) {
			unidentified++
			fmt.Fprintf(stderr, "warning: preserving unidentified file %q: no unambiguous regular version archive identity\n", p)
			continue
		}
		if keep[e.key()] {
			retained++
			continue
		}
		planned = append(planned, candidate{p, info})
		bytes += info.Size()
		fmt.Fprintf(out, "would remove %q bytes=%d\n", p, info.Size())
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	removed := 0
	var errs []error
	if !dry {
		for _, item := range planned {
			if err := ctx.Err(); err != nil {
				errs = append(errs, err)
				break
			}
			if err := safeDirectory(dir, false); err != nil {
				errs = append(errs, err)
				break
			}
			if err := safeDirectory(c.root, false); err != nil {
				errs = append(errs, err)
				break
			}
			current, err := os.Lstat(item.path)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			if !current.Mode().IsRegular() || !os.SameFile(item.info, current) ||
				current.Size() != item.info.Size() || !current.ModTime().Equal(item.info.ModTime()) {
				errs = append(errs, fmt.Errorf("cleanup candidate changed: %q", item.path))
				continue
			}
			if err := os.Remove(item.path); err != nil {
				errs = append(errs, fmt.Errorf("remove %q: %w", item.path, err))
				continue
			}
			removed++
			fmt.Fprintf(out, "removed %q\n", item.path)
			c.data.Entries = slices.DeleteFunc(c.data.Entries, func(e entry) bool {
				p, err := c.destination(e)
				return err == nil && p == item.path
			})
		}
		errs = append(errs, c.save())
	}
	fmt.Fprintf(out, "removed=%d planned=%d retained=%d unidentified=%d reclaimable-bytes=%d dry-run=%t\n",
		removed, len(planned), retained, unidentified, bytes, dry)
	return errors.Join(errs...)
}
