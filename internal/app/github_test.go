package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}
}

func TestTenantRouting(t *testing.T) {
	for _, tt := range []struct {
		name, pin, want string
		status          int
		public          string
		requests        int
	}{
		{"tenant wins", "", "tenant.ghe.com", 200, "", 1},
		{"repository not found permits public", "", "github.com", 404, "public", 2},
		{"private public fallback rejected", "", "", 404, "private", 2},
		{"authentication fails closed", "", "", 401, "", 1},
		{"forbidden fails closed", "", "", 403, "", 1},
		{"tenant pin cannot fall back", "tenant.ghe.com", "tenant.ghe.com", 404, "", 0},
		{"public pin bypasses tenant", "github.com", "github.com", 200, "", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g, _ := newGitHub("tenant.ghe.com")
			count := 0
			c := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				count++
				if r.URL.Host == "api.tenant.ghe.com" {
					return response(tt.status, `{"full_name":"o/r"}`), nil
				}
				return response(200, fmt.Sprintf(`{"visibility":%q,"full_name":"o/r"}`, tt.public)), nil
			})}
			g.clients["tenant.ghe.com"], g.clients["github.com"] = c, c
			host, err := g.route(context.Background(), "o/r", tt.pin)
			if tt.want == "" && err == nil {
				t.Fatal("expected failure")
			}
			if host != tt.want || count != tt.requests {
				t.Fatalf("host %s requests %d, want %s %d", host, count, tt.want, tt.requests)
			}
		})
	}
}

func TestBoundHostFailureDoesNotFallback(t *testing.T) {
	g, _ := newGitHub("tenant.ghe.com")
	count := 0
	g.clients["tenant.ghe.com"] = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		count++
		if r.URL.Host != "api.tenant.ghe.com" {
			t.Fatal("cross-host request")
		}
		return response(404, `{}`), nil
	})}
	_, err := g.commit(context.Background(), "tenant.ghe.com", "o/r", "missing")
	if err == nil || count != 1 || g.clients["github.com"] != nil {
		t.Fatalf("failure did not stay host bound: %v", err)
	}
}

func TestCanonicalRepositoryName(t *testing.T) {
	for _, name := range []string{"Azure/login", "", "Other/login", "../login"} {
		t.Run(name, func(t *testing.T) {
			g, _ := newGitHub("github.com")
			calls := 0
			g.clients[g.home] = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				return response(200, fmt.Sprintf(`{"full_name":%q}`, name)), nil
			})}
			got, err := g.canonicalName(context.Background(), g.home, "azure/login")
			if name != "Azure/login" {
				if err == nil {
					t.Fatal("invalid canonical identity accepted")
				}
				return
			}
			if err != nil || got != name {
				t.Fatalf("canonical name %q: %v", got, err)
			}
			if _, err := g.canonicalName(context.Background(), g.home, "azure/login"); err != nil || calls != 1 {
				t.Fatalf("canonical metadata not reused: calls=%d %v", calls, err)
			}
		})
	}
}

func TestCredentialTransport(t *testing.T) {
	for _, host := range []string{"api.github.com", "codeload.github.com", "evil.example", "api.tenant.ghe.com"} {
		t.Run(host, func(t *testing.T) {
			tr := credentialTransport{host: "github.com", base: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				want := ""
				if host == "api.github.com" {
					want = "token public"
				}
				if r.Header.Get("Authorization") != want || r.Header.Get("Cookie") != "" {
					t.Fatal("credential boundary violated")
				}
				return response(200, ""), nil
			})}
			req, _ := http.NewRequest(http.MethodGet, "https://"+host+"/", nil)
			req.Header.Set("Authorization", "token public")
			req.Header.Set("Cookie", "sensitive")
			r, err := tr.RoundTrip(req)
			if err != nil {
				t.Fatal(err)
			}
			r.Body.Close()
		})
	}
}

func TestGetRetryAndCancellation(t *testing.T) {
	g, _ := newGitHub("github.com")
	calls := 0
	g.clients[g.home] = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			r := response(503, "")
			r.Header.Set("Retry-After", "0")
			return r, nil
		}
		return response(200, "{}"), nil
	})}
	r, err := g.get(context.Background(), g.home, "repos/o/r")
	if err != nil || calls != 2 {
		t.Fatalf("retry failed: %v calls=%d", err, calls)
	}
	r.Body.Close()
	g.clients[g.home].Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, context.Canceled
	})
	_, err = g.get(context.Background(), g.home, "repos/o/r")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
}

func TestRateLimitAndServiceFailuresNeverFallback(t *testing.T) {
	for _, status := range []int{429, 500, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			g, _ := newGitHub("tenant.ghe.com")
			calls := 0
			g.clients[g.home] = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				resp := response(status, "{}")
				resp.Header.Set("Retry-After", "0")
				return resp, nil
			})}
			_, err := g.route(context.Background(), "o/r", "")
			if err == nil || calls != 3 || g.clients["github.com"] != nil {
				t.Fatalf("status %d crossed hosts or ignored error: %v calls=%d", status, err, calls)
			}
		})
	}
}

func TestTransportFailureNeverFallsBack(t *testing.T) {
	g, _ := newGitHub("tenant.ghe.com")
	g.clients[g.home] = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, errors.New("network unavailable")
	})}
	if _, err := g.route(context.Background(), "o/r", ""); err == nil || g.clients["github.com"] != nil {
		t.Fatal("transport failure triggered fallback")
	}
}

func TestTagPagination(t *testing.T) {
	g, _ := newGitHub("github.com")
	calls := 0
	g.clients[g.home] = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if calls == 1 {
			return response(200, "["+strings.TrimSuffix(strings.Repeat(`{"name":"v1"},`, 100), ",")+"]"), nil
		}
		return response(200, `[{"name":"v2"}]`), nil
	})}
	tags, err := g.tags(context.Background(), g.home, "o/r")
	if err != nil || len(tags) != 101 || calls != 2 {
		t.Fatalf("pagination failed %d %d %v", len(tags), calls, err)
	}
}
