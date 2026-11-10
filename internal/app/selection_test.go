package app

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestSelectTags(t *testing.T) {
	tags := []string{"main", "v2-beta", "v10", "v10.1", "v10.1.0", "v10.1.1", "v2", "v2.3.9", "v2.4.0", "v1", "v1.8.0"}
	for _, tt := range []struct {
		name        string
		majors, per int
		want        []string
	}{
		{"numeric majors and tags", 2, 1, []string{"v10.1.1", "v10", "v2.4.0", "v2"}},
		{"minor aliases count as tags", 1, 3, []string{"v10.1.1", "v10.1.0", "v10.1", "v10"}},
		{"zero means unlimited", 0, 0, []string{"v10.1.1", "v10.1.0", "v10.1", "v10", "v2.4.0", "v2.3.9", "v2", "v1.8.0", "v1"}},
		{"all majors one version", 0, 1, []string{"v10.1.1", "v10", "v2.4.0", "v2", "v1.8.0", "v1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectTags(tags, tt.majors, tt.per)
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("got %v, %v; want %v", got, err, tt.want)
			}
		})
	}
	if _, err := selectTags(tags, -1, 0); err == nil {
		t.Fatal("negative limits accepted")
	}
	got, err := selectTags([]string{"v1.0", "v1.0", "v9999999999999999999999999", "1.0", "v1.0-rc"}, 0, 0)
	if err != nil || !reflect.DeepEqual(got, []string{"v1.0"}) {
		t.Fatalf("invalid versions/duplicates not filtered: %v %v", got, err)
	}
}

func TestActionInput(t *testing.T) {
	for _, s := range []string{"checkout", "../a@v1", "a/..", "a/r@", "a/r/p/../x@v1", "a/r@a\nb", "a/r@v1@x", "a/r/p\\x@main"} {
		if _, _, err := actionInput(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
	repo, ref, err := actionInput("Actions/Cache/save@feature/x")
	if err != nil || repo != "actions/cache" || ref != "feature/x" {
		t.Fatalf("%s %s %v", repo, ref, err)
	}
}

func lockedFixture(version, host, commit string) []byte {
	hostLine := ""
	if host != "" {
		hostLine = "    hostname: " + host + "\n"
	}
	ref := "    ref: v1\n"
	if version == "v0.0.1" {
		ref = "    tag: v1\n"
	}
	return []byte(fmt.Sprintf("version: %s\nworkflows:\n  .github/workflows/ci.yml:\n    - o/r@v1\ndependencies:\n  o/r@v1:\n%s%s    commit: %s\n    owner_id: 1\n    repo_id: 2\n", version, hostLine, ref, commit))
}

func TestLockedHostsAndDigests(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, tt := range []struct {
		version, explicit, want string
	}{
		{"v0.0.1", "", "github.com"},
		{"v0.0.2", "", "github.com"},
		{"v0.0.3", "", "tenant.ghe.com"},
		{"v0.0.3", "github.com", "github.com"},
		{"v0.0.3", "tenant.ghe.com", "tenant.ghe.com"},
	} {
		t.Run(tt.version+tt.explicit, func(t *testing.T) {
			g, _ := newGitHub("tenant.ghe.com")
			es, err := g.locked(lockedFixture(tt.version, tt.explicit, "sha1-"+sha), []string{"zip", "tar.gz"})
			if err != nil {
				t.Fatal(err)
			}

			if len(es) != 2 || es[0].Host != tt.want || es[0].SHA != sha || !es[0].Pinned {
				t.Fatalf("wrong normalized entries: %+v", es)
			}
		})
	}
	g, _ := newGitHub("tenant.ghe.com")
	for _, data := range [][]byte{
		lockedFixture("v99.0.0", "", "sha1-"+sha),
		lockedFixture("v0.0.3", "other.ghe.com", "sha1-"+sha),
		lockedFixture("v0.0.3", "", "sha256-"+strings.Repeat("a", 64)),
		[]byte("version: [broken"),
		lockedFixture("v0.0.3", "", "sha1-short"),
	} {
		if _, err := g.locked(data, []string{"zip"}); err == nil {
			t.Fatalf("accepted invalid lockfile %s", data)
		}
	}
}

func TestLockfileTransitiveDependenciesAndHostConflicts(t *testing.T) {
	g, _ := newGitHub("tenant.ghe.com")
	sha := strings.Repeat("a", 40)
	data := string(lockedFixture("v0.0.3", "", "sha1-"+sha))
	data += "    uses:\n      - o/sub@v2\n  o/sub@v2:\n    ref: v2\n    commit: sha1-" + sha + "\n    owner_id: 1\n    repo_id: 3\n"
	es, err := g.locked([]byte(data), []string{"zip"})
	if err != nil || len(es) != 2 || es[1].Repo != "o/sub" {
		t.Fatalf("transitive dependency missing: %+v %v", es, err)
	}
	missing := strings.Replace(data, "      - o/sub@v2", "      - o/missing@v1", 1)
	if _, err := g.locked([]byte(missing), []string{"zip"}); err == nil {
		t.Fatal("missing nested dependency accepted")
	}
	conflicting := strings.Replace(data, "o/sub@v2", "o/r@v2", -1)
	conflicting = strings.Replace(conflicting, "  o/r@v2:\n", "  o/r@v2:\n    hostname: github.com\n", 1)
	if _, err := g.locked([]byte(conflicting), []string{"zip"}); err == nil {
		t.Fatal("same repository on two hosts accepted")
	}
}
