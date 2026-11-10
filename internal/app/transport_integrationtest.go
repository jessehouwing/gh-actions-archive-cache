//go:build integrationtest

package app

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
)

type fixtureRoundTripper struct{ target *url.URL }

func (t fixtureRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.URL = new(url.URL)
	*r.URL = *req.URL
	r.URL.Scheme, r.URL.Host = t.target.Scheme, t.target.Host
	r.Host = t.target.Host
	r.Header = req.Header.Clone()
	r.Header.Set("X-Fixture-Host", req.URL.Host)
	return http.DefaultTransport.RoundTrip(r)
}

func init() {
	if endpoint := os.Getenv("ARCHIVE_CACHE_TEST_ENDPOINT"); endpoint != "" {
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "http" || net.ParseIP(u.Hostname()) == nil ||
			!net.ParseIP(u.Hostname()).IsLoopback() {
			panic("integration fixture endpoint must be loopback HTTP")
		}
		fixtureTransport = fixtureRoundTripper{u}
	}
	fixtureConfigRoot = os.Getenv("ARCHIVE_CACHE_TEST_CONFIG_ROOT")
	if fixtureConfigRoot != "" {
		persistRegistry = func(scope, value string, dry bool) error {
			if dry {
				return nil
			}
			return os.WriteFile(filepath.Join(fixtureConfigRoot, "registry-"+scope), []byte(fmt.Sprintf("%s=%s\n", cacheVariable, value)), 0o600)
		}
	}
}
