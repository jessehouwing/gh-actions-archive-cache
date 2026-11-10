package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"

	lockfile "github.com/github/actions-lockfile/go/pkg/lockfile"
)

type entry struct {
	Host   string `json:"host"`
	Repo   string `json:"repo"`
	Name   string `json:"name,omitempty"`
	SHA    string `json:"sha"`
	Ref    string `json:"ref"`
	Format string `json:"format"`
	Exact  bool   `json:"exact,omitempty"`
	Pinned bool   `json:"pinned,omitempty"`
}

func (e entry) key() string { return e.Repo + "/" + e.SHA + "." + e.Format }

type version struct {
	tag                 string
	major, minor, patch uint64
	parts               int
}

var versionPattern = regexp.MustCompile(`^v(\d+)(?:\.(\d+))?(?:\.(\d+))?$`)

func parseVersion(tag string) (version, bool) {
	m := versionPattern.FindStringSubmatch(tag)
	if m == nil {
		return version{}, false
	}
	v := version{tag: tag}
	values := []*uint64{&v.major, &v.minor, &v.patch}
	for i := 1; i < len(m) && m[i] != ""; i++ {
		n, err := strconv.ParseUint(m[i], 10, 64)
		if err != nil || n > math.MaxInt64 {
			return version{}, false
		}
		*values[i-1] = n
		v.parts++
	}
	return v, true
}

func selectTags(tags []string, majors, perMajor int) ([]string, error) {
	if majors < 0 || perMajor < 0 {
		return nil, errors.New("version limits must be nonnegative (0 means unlimited)")
	}
	var versions []version
	for _, tag := range tags {
		if v, ok := parseVersion(tag); ok {
			versions = append(versions, v)
		}
	}
	slices.SortFunc(versions, func(a, b version) int {
		if c := cmpNumber(b.major, a.major); c != 0 {
			return c
		}
		if c := cmpNumber(b.minor, a.minor); c != 0 {
			return c
		}
		if c := cmpNumber(b.patch, a.patch); c != 0 {
			return c
		}
		// Upstream treats a missing component as -1.
		if c := b.parts - a.parts; c != 0 {
			return c
		}
		return strings.Compare(a.tag, b.tag)
	})
	seenMajors := map[uint64]bool{}
	counts := map[uint64]int{}
	seenTags := map[string]bool{}
	var selected []string
	for _, v := range versions {
		if seenTags[v.tag] {
			continue
		}
		seenTags[v.tag] = true
		if !seenMajors[v.major] {
			if majors > 0 && len(seenMajors) >= majors {
				continue
			}
			seenMajors[v.major] = true
		}
		if v.parts == 1 || perMajor == 0 || counts[v.major] < perMajor {
			selected = append(selected, v.tag)
			if v.parts > 1 {
				counts[v.major]++
			}
		}
	}
	return selected, nil
}

func cmpNumber(a, b uint64) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func actionInput(input string) (repo, ref string, err error) {
	path, ref, hasRef := strings.Cut(input, "@")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid action %q; expected OWNER/REPO[/PATH][@REF]", input)
	}
	repo = strings.ToLower(strings.Join(parts[:2], "/"))
	if !repoPattern.MatchString(repo) || parts[1] == "." || parts[1] == ".." {
		return "", "", fmt.Errorf("invalid repository %q", repo)
	}
	for _, p := range parts[2:] {
		if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "\\\r\n\x00") {
			return "", "", fmt.Errorf("invalid action subpath %q", path)
		}
	}
	if (hasRef && ref == "") || strings.ContainsAny(ref, "\r\n\x00@") {
		return "", "", errors.New("invalid or empty action ref")
	}
	return repo, ref, nil
}

func (g *github) resolve(ctx context.Context, input string, majors, perMajor int, formats []string) ([]entry, error) {
	repo, ref, err := actionInput(input)
	if err != nil {
		return nil, err
	}
	host, err := g.route(ctx, repo, "")
	if err != nil {
		return nil, err
	}
	refs := []string{ref}
	if ref == "" {
		tags, err := g.tags(ctx, host, repo)
		if err != nil {
			return nil, err
		}
		refs, err = selectTags(tags, majors, perMajor)
		if err != nil {
			return nil, err
		}
		if len(refs) == 0 {
			return nil, fmt.Errorf("%s has no matching stable version tags", repo)
		}
	}
	var entries []entry
	for _, r := range refs {
		sha, err := g.commit(ctx, host, repo, r)
		if err != nil {
			return nil, err
		}
		for _, format := range formats {
			entries = append(entries, entry{Host: host, Repo: repo, Name: g.names[host+"/"+repo], SHA: sha, Ref: r, Format: format, Exact: ref != ""})
		}
	}
	return entries, nil
}

func (g *github) locked(data []byte, formats []string) ([]entry, error) {
	f, err := lockfile.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parse lockfile: %w", err)
	}
	hosts := map[string]string{}
	var entries []entry
	keys := make([]string, 0, len(f.Dependencies))
	for key := range f.Dependencies {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		dep := f.Dependencies[key]
		for _, nested := range dep.Uses {
			if _, found := f.Dependencies[nested]; !found {
				return nil, fmt.Errorf("%s references missing dependency %s", key, nested)
			}
		}
		pin, ok := lockfile.ParsePin(key)
		if !ok {
			return nil, fmt.Errorf("invalid dependency %q", key)
		}
		repo := strings.ToLower(pin.NWO)
		host := dep.Hostname
		if host == "" {
			host = g.home
		}
		if _, err := g.route(context.Background(), repo, host); err != nil {
			return nil, err
		}
		if prev := hosts[repo]; prev != "" && prev != host {
			return nil, fmt.Errorf("%s is locked on both %s and %s", repo, prev, host)
		}
		hosts[repo] = host
		algo, sha, ok := strings.Cut(dep.Commit, "-")
		if !ok || algo != "sha1" || !shaPattern.MatchString(sha) {
			return nil, fmt.Errorf("%s has unsupported commit digest %q; expected sha1-<40 hex digits>", key, dep.Commit)
		}
		for _, format := range formats {
			entries = append(entries, entry{Host: host, Repo: repo, SHA: sha, Ref: dep.Ref, Format: format, Pinned: true})
		}
	}
	for workflow, pins := range f.Workflows {
		for _, pin := range pins {
			if _, found := f.Dependencies[pin]; !found {
				return nil, fmt.Errorf("%s references missing dependency %s", workflow, pin)
			}
		}
	}
	return entries, nil
}
