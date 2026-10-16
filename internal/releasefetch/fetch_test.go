package releasefetch_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sofired/treerig/internal/releasefetch"
	"github.com/sofired/treerig/internal/releasefetch/releasetest"
)

func TestHTTPGet(t *testing.T) {
	t.Run("returns the body of an https URL", func(t *testing.T) {
		r := releasetest.New(t, "1.2.3")
		got, err := r.Fetcher().Get(context.Background(), r.Base()+"checksums.txt", releasefetch.MaxChecksums)
		if err != nil || string(got) != string(r.File("checksums.txt")) {
			t.Fatalf("Get = %q, %v", got, err)
		}
	})

	t.Run("refuses a URL that is not https, without a request", func(t *testing.T) {
		var hit bool
		plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
		defer plain.Close()
		_, err := releasefetch.NewHTTP(plain.Client()).Get(context.Background(), plain.URL+"/x", 10)
		if err == nil || !strings.Contains(err.Error(), "only https URLs are allowed") || hit {
			t.Fatalf("err = %v, hit = %v", err, hit)
		}
	})

	t.Run("a 404 is a StatusError", func(t *testing.T) {
		r := releasetest.New(t, "1.2.3")
		_, err := r.Fetcher().Get(context.Background(), r.Base()+"missing", 10)
		var status *releasefetch.StatusError
		if !errors.As(err, &status) || status.Status != http.StatusNotFound || !strings.HasSuffix(status.URL, "/missing") {
			t.Fatalf("err = %#v", err)
		}
	})

	t.Run("a body larger than the cap is an error, announced or not", func(t *testing.T) {
		r := releasetest.New(t, "1.2.3")
		r.Set("big", []byte(strings.Repeat("x", 100)))
		_, err := r.Fetcher().Get(context.Background(), r.Base()+"big", 99)
		var large *releasefetch.TooLargeError
		if !errors.As(err, &large) || large.Limit != 99 {
			t.Fatalf("announced length: err = %#v", err)
		}
		// Chunked, with no Content-Length to check early.
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			for i := 0; i < 10; i++ {
				_, _ = w.Write([]byte(strings.Repeat("y", 20)))
				w.(http.Flusher).Flush()
			}
		}))
		defer server.Close()
		_, err = releasefetch.NewHTTP(server.Client()).Get(context.Background(), server.URL, 99)
		if !errors.As(err, &large) {
			t.Fatalf("chunked: err = %#v", err)
		}
		got, err := releasefetch.NewHTTP(server.Client()).Get(context.Background(), server.URL, 200)
		if err != nil || len(got) != 200 {
			t.Fatalf("a body exactly at the cap: %d bytes, %v", len(got), err)
		}
	})

	t.Run("stops reading an endless body at the cap instead of holding it", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			chunk := []byte(strings.Repeat("e", 4096))
			for req.Context().Err() == nil {
				if _, err := w.Write(chunk); err != nil {
					return
				}
			}
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, err := releasefetch.NewHTTP(server.Client()).Get(ctx, server.URL, 10000)
		var large *releasefetch.TooLargeError
		if !errors.As(err, &large) || ctx.Err() != nil {
			t.Fatalf("err = %v, ctx = %v", err, ctx.Err())
		}
	})

	t.Run("refuses a redirect to http, and never reaches it", func(t *testing.T) {
		var hit bool
		plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
		defer plain.Close()
		r := releasetest.New(t, "1.2.3")
		r.Redirect("checksums.txt", plain.URL+"/checksums.txt")
		_, err := r.Fetcher().Get(context.Background(), r.Base()+"checksums.txt", 1<<20)
		if err == nil || !strings.Contains(err.Error(), "only https is allowed") || hit {
			t.Fatalf("err = %v, hit = %v", err, hit)
		}
	})

	t.Run("refuses a redirect to http even through a client that would follow it", func(t *testing.T) {
		var hit bool
		plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hit = true }))
		defer plain.Close()
		r := releasetest.New(t, "1.2.3")
		r.Redirect("checksums.txt", plain.URL+"/")
		client := r.Server.Client()
		client.CheckRedirect = nil // the default policy follows anything
		if _, err := releasefetch.NewHTTP(client).Get(context.Background(), r.Base()+"checksums.txt", 10); err == nil || hit {
			t.Fatalf("err = %v, hit = %v", err, hit)
		}
	})

	t.Run("follows a redirect to another https URL", func(t *testing.T) {
		r := releasetest.New(t, "1.2.3")
		r.Redirect("alias", r.Base()+"checksums.txt")
		got, err := r.Fetcher().Get(context.Background(), r.Base()+"alias", 1<<20)
		if err != nil || string(got) != string(r.File("checksums.txt")) {
			t.Fatalf("Get = %q, %v", got, err)
		}
	})

	t.Run("asks for the identity encoding, so a compressed body is not expanded behind the cap", func(t *testing.T) {
		var seen string
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			seen = req.Header.Get("Accept-Encoding")
		}))
		defer server.Close()
		if _, err := releasefetch.NewHTTP(server.Client()).Get(context.Background(), server.URL, 10); err != nil {
			t.Fatal(err)
		}
		if seen != "identity" {
			t.Fatalf("Accept-Encoding = %q", seen)
		}
	})

	t.Run("stops when the context ends", func(t *testing.T) {
		release := make(chan struct{})
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			select {
			case <-release:
			case <-req.Context().Done():
			}
		}))
		defer server.Close()
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := releasefetch.NewHTTP(server.Client()).Get(ctx, server.URL, 10); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestLatest(t *testing.T) {
	t.Run("reads the version from the archive names and discards the bytes", func(t *testing.T) {
		r := releasetest.New(t, "1.2.3")
		r.SetLatest("1.2.3")
		got, err := releasefetch.Latest(context.Background(), r.Fetcher(), r.Source())
		if err != nil || got != "1.2.3" {
			t.Fatalf("Latest = %q, %v", got, err)
		}
		if reqs := r.Requests(); len(reqs) != 1 || reqs[0] != "/releases/latest/download/checksums.txt" {
			t.Fatalf("requests = %v", reqs)
		}
	})

	t.Run("a repository with no release that is not a pre-release answers 404, which is ErrNoStableRelease", func(t *testing.T) {
		r := releasetest.New(t, "1.2.3")
		_, err := releasefetch.Latest(context.Background(), r.Fetcher(), r.Source())
		if !errors.Is(err, releasefetch.ErrNoStableRelease) {
			t.Fatalf("err = %v", err)
		}
	})

	t.Run("refuses archives of more than one version, and a version that is not a release", func(t *testing.T) {
		for name, body := range map[string]string{
			"two versions":  strings.Repeat("a", 64) + "  treerig_1.2.3_linux_amd64.tar.gz\n" + strings.Repeat("b", 64) + "  treerig_1.2.4_linux_arm64.tar.gz\n",
			"not a release": strings.Repeat("a", 64) + "  treerig_v1.2_linux_amd64.tar.gz\n",
			"leading zero":  strings.Repeat("a", 64) + "  treerig_01.2.3_linux_amd64.tar.gz\n",
			"no archive":    strings.Repeat("a", 64) + "  treerig_1.2.3_amd64.deb\n",
			"empty":         "",
		} {
			t.Run(name, func(t *testing.T) {
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
				defer server.Close()
				_, err := releasefetch.Latest(context.Background(), releasefetch.NewHTTP(server.Client()), releasefetch.Source{Repository: server.URL})
				if err == nil || errors.Is(err, releasefetch.ErrNoStableRelease) {
					t.Fatalf("err = %v", err)
				}
			})
		}
	})

	t.Run("accepts a pre-release version in the names", func(t *testing.T) {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(strings.Repeat("a", 64) + "  treerig_0.1.0-rc.1_darwin_arm64.tar.gz\n"))
		}))
		defer server.Close()
		got, err := releasefetch.Latest(context.Background(), releasefetch.NewHTTP(server.Client()), releasefetch.Source{Repository: server.URL})
		if err != nil || got != "0.1.0-rc.1" {
			t.Fatalf("Latest = %q, %v", got, err)
		}
	})
}

func TestSource(t *testing.T) {
	t.Run("the zero value is the treerig repository, and a base ends /releases/download/v<release>/", func(t *testing.T) {
		var s releasefetch.Source
		if got := s.ReleaseBase("0.1.0-rc.1"); got != "https://github.com/sofired/treerig/releases/download/v0.1.0-rc.1/" {
			t.Fatalf("ReleaseBase = %q", got)
		}
		if got := s.LatestChecksumsURL(); got != "https://github.com/sofired/treerig/releases/latest/download/checksums.txt" {
			t.Fatalf("LatestChecksumsURL = %q", got)
		}
		if got := (releasefetch.Source{Repository: "https://h:1/"}).ReleaseBase("1.2.3"); got != "https://h:1/releases/download/v1.2.3/" {
			t.Fatalf("ReleaseBase = %q", got)
		}
	})
}

func TestRequireHTTPS(t *testing.T) {
	https, _ := http.NewRequest(http.MethodGet, "https://example.test/x", nil)
	plain, _ := http.NewRequest(http.MethodGet, "http://example.test/x", nil)
	via := func(n int) []*http.Request { return make([]*http.Request, n) }

	t.Run("follows an https redirect up to the tenth hop and stops there", func(t *testing.T) {
		if err := releasefetch.RequireHTTPS(https, via(9)); err != nil {
			t.Fatalf("9 earlier requests: %v", err)
		}
		if err := releasefetch.RequireHTTPS(https, via(10)); err == nil || !strings.Contains(err.Error(), "10 redirects") {
			t.Fatalf("10 earlier requests: %v", err)
		}
	})

	t.Run("refuses any other scheme at any hop", func(t *testing.T) {
		for _, n := range []int{0, 5, 9} {
			if err := releasefetch.RequireHTTPS(plain, via(n)); err == nil || !strings.Contains(err.Error(), "only https is allowed") {
				t.Fatalf("%d earlier requests: %v", n, err)
			}
		}
	})
}
