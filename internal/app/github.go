package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/cli/go-gh/v2/pkg/api"
	"github.com/cli/go-gh/v2/pkg/auth"
	"github.com/cli/go-gh/v2/pkg/config"
)

var tenantPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.ghe\.com$`)
var repoPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*/[A-Za-z0-9_.-]+$`)
var shaPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// fixtureTransport is set only by integration-tagged builds.
var fixtureTransport http.RoundTripper

type github struct {
	home    string
	clients map[string]*http.Client
	routes  map[string]string
	names   map[string]string
}

type httpError struct {
	Status int
	Host   string
	Path   string
}

func (e *httpError) Error() string {
	return fmt.Sprintf("%s %s: HTTP %d (check gh authentication, repository access, and rate limits)", e.Host, e.Path, e.Status)
}

func newGitHub(host string) (*github, error) {
	if host == "" {
		host, _ = auth.DefaultHost()
	}
	host = strings.ToLower(host)
	if host != "github.com" && !tenantPattern.MatchString(host) {
		return nil, fmt.Errorf("unsupported hostname %q: use github.com or a tenant.ghe.com host", host)
	}
	return &github{home: host, clients: map[string]*http.Client{}, routes: map[string]string{}, names: map[string]string{}}, nil
}

type credentialTransport struct {
	base http.RoundTripper
	host string
}

func (t credentialTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header = req.Header.Clone()
	r.Header.Del("Cookie")
	if r.URL.Scheme != "https" || r.URL.Host != "api."+t.host ||
		r.Header.Get("Authorization") == "token anonymous" || r.Header.Get("Authorization") == "Bearer anonymous" {
		r.Header.Del("Authorization")
	}
	return t.base.RoundTrip(r)
}

func (g *github) client(host string) (*http.Client, error) {
	if c := g.clients[host]; c != nil {
		return c, nil
	}
	token, _ := auth.TokenForHost(host)
	if host == "github.com" && tenantPattern.MatchString(g.home) {
		// go-gh accepts GH_TOKEN for both GHE.com and github.com. A tenant
		// invocation must not forward that shared environment token publicly.
		token = publicToken()
	}
	if token == "" {
		if host != "github.com" {
			return nil, fmt.Errorf("no authentication for %s; run gh auth login --hostname %s", host, host)
		}
		token = "anonymous"
	}
	base := fixtureTransport
	if base == nil {
		base = http.DefaultTransport
	}
	c, err := api.NewHTTPClient(api.ClientOptions{
		Host: host, AuthToken: token, Transport: credentialTransport{base, host},
		Timeout: 2 * time.Minute, LogIgnoreEnv: true,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("too many archive redirects")
			}
			if req.URL.Scheme != "https" {
				return errors.New("refusing non-HTTPS redirect")
			}
			if req.URL.Host != via[0].URL.Host {
				req.Header.Del("Authorization")
				req.Header.Del("Cookie")
			}
			return nil
		},
	})
	if err != nil {
		return nil, fmt.Errorf("create %s client: %w", host, err)
	}
	g.clients[host] = c
	return c, nil
}

func publicToken() string {
	if token := os.Getenv("GH_ACTIONS_ARCHIVE_CACHE_GITHUB_COM_TOKEN"); token != "" {
		return token
	}
	if cfg, err := config.Read(nil); err == nil {
		if token, err := cfg.Get([]string{"hosts", "github.com", "oauth_token"}); err == nil && token != "" {
			return token
		}
	}
	gh := os.Getenv("GH_PATH")
	if gh == "" {
		gh = "gh"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, gh, "auth", "token", "--secure-storage", "--hostname", "github.com").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func apiURL(host, path string) string { return "https://api." + host + "/" + path }

func (g *github) get(ctx context.Context, host, path string) (*http.Response, error) {
	c, err := g.client(host)
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 3; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL(host, path), nil)
		if err != nil {
			return nil, err
		}
		r, err := c.Do(req)
		if err != nil {
			// Do not expose URLs containing signed query strings from redirect errors.
			return nil, fmt.Errorf("request to %s failed: %w", host, sanitizedNetworkError(err))
		}
		if r.StatusCode >= 200 && r.StatusCode < 300 {
			return r, nil
		}
		_, drainErr := io.Copy(io.Discard, io.LimitReader(r.Body, 4096))
		closeErr := r.Body.Close()
		if drainErr != nil || closeErr != nil {
			return nil, fmt.Errorf("read %s error response: %w", host, errors.Join(drainErr, closeErr))
		}
		retry := r.StatusCode == 429 || r.StatusCode >= 500 ||
			(r.StatusCode == 403 && r.Header.Get("X-RateLimit-Remaining") == "0")
		if !retry || attempt == 2 {
			return nil, &httpError{r.StatusCode, host, strings.Split(path, "?")[0]}
		}
		delay := time.Duration(attempt+1) * time.Second
		if value := r.Header.Get("Retry-After"); value != "" {
			if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
				delay = time.Duration(seconds) * time.Second
			} else if until, err := http.ParseTime(value); err == nil {
				delay = time.Until(until)
			}
		} else if reset, err := strconv.ParseInt(r.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			delay = time.Until(time.Unix(reset, 0))
		}
		if delay > 30*time.Second {
			return nil, fmt.Errorf("%s rate limited; retry after %s", host, delay.Round(time.Second))
		}
		if delay < 0 {
			delay = 0
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}
	return nil, errors.New("request retry budget exhausted")
}

func sanitizedNetworkError(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return sanitizedNetworkError(u.Err)
	}
	return err
}

func (g *github) json(ctx context.Context, host, path string, out any) error {
	r, err := g.get(ctx, host, path)
	if err != nil {
		return err
	}
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(out); err != nil {
		return fmt.Errorf("decode %s response: %w", host, err)
	}
	return nil
}

func (g *github) route(ctx context.Context, repo, pinned string) (string, error) {
	if pinned != "" && pinned != g.home && !(tenantPattern.MatchString(g.home) && pinned == "github.com") {
		return "", fmt.Errorf("%s pinned to unsupported host %s for home %s", repo, pinned, g.home)
	}
	if pinned != "" {
		return pinned, nil
	}
	if host := g.routes[repo]; host != "" {
		return host, nil
	}
	host := g.home
	var meta struct {
		Visibility string `json:"visibility"`
		FullName   string `json:"full_name"`
	}
	err := g.json(ctx, host, "repos/"+repo, &meta)
	if err != nil && tenantPattern.MatchString(host) {
		var he *httpError
		if !errors.As(err, &he) || he.Status != 404 {
			return "", err
		}
		host = "github.com"
		err = g.json(ctx, host, "repos/"+repo, &meta)
		if err == nil && meta.Visibility != "public" {
			return "", fmt.Errorf("%s is not a public github.com fallback repository", repo)
		}
	}
	if err != nil {
		return "", err
	}
	if err := g.rememberName(host, repo, meta.FullName); err != nil {
		return "", err
	}
	g.routes[repo] = host
	return host, nil
}

func (g *github) rememberName(host, repo, name string) error {
	if !repoPattern.MatchString(name) || !strings.EqualFold(name, repo) {
		return fmt.Errorf("%s repository resolved to %q; use its current canonical name explicitly", repo, name)
	}
	g.names[host+"/"+repo] = name
	return nil
}

func (g *github) canonicalName(ctx context.Context, host, repo string) (string, error) {
	if name := g.names[host+"/"+repo]; name != "" {
		return name, nil
	}
	var meta struct {
		FullName string `json:"full_name"`
	}
	if err := g.json(ctx, host, "repos/"+repo, &meta); err != nil {
		return "", err
	}
	if err := g.rememberName(host, repo, meta.FullName); err != nil {
		return "", err
	}
	return g.names[host+"/"+repo], nil
}

func (g *github) commit(ctx context.Context, host, repo, ref string) (string, error) {
	var result struct {
		SHA string `json:"sha"`
	}
	if err := g.json(ctx, host, "repos/"+repo+"/commits/"+url.PathEscape(ref), &result); err != nil {
		return "", err
	}
	if !shaPattern.MatchString(result.SHA) {
		return "", fmt.Errorf("%s@%s returned invalid commit SHA", repo, ref)
	}
	return result.SHA, nil
}

func (g *github) tags(ctx context.Context, host, repo string) ([]string, error) {
	var all []string
	for page := 1; page <= 10000; page++ {
		var tags []struct {
			Name string `json:"name"`
		}
		if err := g.json(ctx, host, fmt.Sprintf("repos/%s/tags?per_page=100&page=%d", repo, page), &tags); err != nil {
			return nil, err
		}
		for _, t := range tags {
			all = append(all, t.Name)
		}
		if len(tags) < 100 {
			return all, nil
		}
	}
	return nil, errors.New("tag pagination exceeds 10000 pages")
}
