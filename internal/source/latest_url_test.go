package source

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestLatestURLDiscoverer_RedirectTokenIsResolvedURL(t *testing.T) {
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	latest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, target.URL+"/foo-1.2.3.deb", http.StatusFound)
	}))
	defer latest.Close()

	d, err := NewLatestURLDiscoverer(latest.URL+"/foo-latest.deb", latest.Client())
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.HasSuffix(res.Probe.URL, "/foo-1.2.3.deb") {
		t.Fatalf("URL = %q, want suffix /foo-1.2.3.deb", res.Probe.URL)
	}
	if res.Probe.Token != res.Probe.URL {
		t.Fatalf("token = %q, want resolved URL %q", res.Probe.Token, res.Probe.URL)
	}
	if res.Unchanged {
		t.Fatalf("Unchanged = true on cold start")
	}
}

func TestLatestURLDiscoverer_TokenFromETagWhenNoRedirect(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"abc123"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d, _ := NewLatestURLDiscoverer(srv.URL+"/x.deb", srv.Client())
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.Token != `"abc123"` {
		t.Fatalf("token = %q, want ETag", res.Probe.Token)
	}
	if res.Probe.URL != srv.URL+"/x.deb" {
		t.Fatalf("URL = %q, want configured URL", res.Probe.URL)
	}
}

func TestLatestURLDiscoverer_TokenFromLastModifiedWhenNoETag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Last-Modified", "Wed, 21 Oct 2026 07:28:00 GMT")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d, _ := NewLatestURLDiscoverer(srv.URL+"/x.deb", srv.Client())
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.Token != "Wed, 21 Oct 2026 07:28:00 GMT" {
		t.Fatalf("token = %q, want Last-Modified", res.Probe.Token)
	}
}

func TestLatestURLDiscoverer_EmptyTokenWhenNoSignals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d, _ := NewLatestURLDiscoverer(srv.URL+"/x.deb", srv.Client())
	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.Token != "" {
		t.Fatalf("token = %q, want empty (always-fetch)", res.Probe.Token)
	}
	// With empty token, prev.Token cannot match → never Unchanged.
	res2, err := d.Probe(context.Background(), ProbeInput{Prev: &Probe{Token: ""}})
	if err != nil {
		t.Fatalf("probe2: %v", err)
	}
	if res2.Unchanged {
		t.Fatalf("empty token must never be Unchanged")
	}
}

func TestLatestURLDiscoverer_UnchangedWhenTokenMatches(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("ETag", `"v9"`)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d, _ := NewLatestURLDiscoverer(srv.URL+"/x.deb", srv.Client())
	res, err := d.Probe(context.Background(), ProbeInput{Prev: &Probe{Token: `"v9"`}})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !res.Unchanged {
		t.Fatalf("Unchanged = false, want true")
	}
}

func TestLatestURLDiscoverer_NonSuccessIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d, _ := NewLatestURLDiscoverer(srv.URL+"/missing.deb", srv.Client())
	if _, err := d.Probe(context.Background(), ProbeInput{}); err == nil {
		t.Fatalf("expected error on 404")
	}
}

func TestLatestURLDiscoverer_S3ChecksumPopulatesAssetDigest(t *testing.T) {
	body := []byte("the .deb bytes")
	sum := sha256.Sum256(body)
	wantHex := hex.EncodeToString(sum[:])
	b64 := base64.StdEncoding.EncodeToString(sum[:])

	var sawChecksumMode string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawChecksumMode = r.Header.Get("x-amz-checksum-mode")
		w.Header().Set("x-amz-checksum-sha256", b64)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d, _ := NewLatestURLDiscoverer(srv.URL+"/x.deb", srv.Client())
	d.isS3 = func(*url.URL) bool { return true } // pretend the test server is S3

	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if sawChecksumMode != "ENABLED" {
		t.Fatalf("x-amz-checksum-mode header = %q, want ENABLED", sawChecksumMode)
	}
	if res.Probe.AssetDigest != wantHex {
		t.Fatalf("AssetDigest = %q, want %q", res.Probe.AssetDigest, wantHex)
	}
}

func TestLatestURLDiscoverer_NonS3HostIgnoresChecksumHeader(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-amz-checksum-sha256", "Z3JlZW4gZWdncyBhbmQgaGFt")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d, _ := NewLatestURLDiscoverer(srv.URL+"/x.deb", srv.Client())
	// Default isS3 → false for httptest URL, so digest must stay empty even
	// though the response carries an x-amz-checksum-sha256 header.

	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.AssetDigest != "" {
		t.Fatalf("AssetDigest = %q, want empty for non-S3 host", res.Probe.AssetDigest)
	}
}

func TestLatestURLDiscoverer_S3MalformedChecksumLeavesDigestEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("x-amz-checksum-sha256", "not base64!!!")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d, _ := NewLatestURLDiscoverer(srv.URL+"/x.deb", srv.Client())
	d.isS3 = func(*url.URL) bool { return true }

	res, err := d.Probe(context.Background(), ProbeInput{})
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if res.Probe.AssetDigest != "" {
		t.Fatalf("AssetDigest = %q, want empty for malformed header", res.Probe.AssetDigest)
	}
}

func TestDefaultIsS3Host(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
	}{
		{"https://bucket.s3.amazonaws.com/key", true},
		{"https://bucket.s3.us-east-1.amazonaws.com/key", true},
		{"https://s3.amazonaws.com/bucket/key", true},
		{"https://s3.us-west-2.amazonaws.com/bucket/key", true},
		{"https://s3-website-us-east-1.amazonaws.com/bucket/key", true},
		{"https://ec2.amazonaws.com/foo", false},
		{"https://something-s3y.amazonaws.com/foo", false},
		{"https://example.com/x", false},
	}
	for _, tc := range cases {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("parse %s: %v", tc.raw, err)
		}
		if got := defaultIsS3Host(u); got != tc.want {
			t.Errorf("defaultIsS3Host(%s) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}
