package fetch

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// buildFakeDeb produces a minimal valid .deb byte slice with the given
// control stanza. The resulting layout: ar magic, debian-binary member,
// control.tar.gz member, data.tar.gz member.
func buildFakeDeb(t *testing.T, controlStanza string) []byte {
	t.Helper()

	// 1. control.tar.gz containing ./control with controlStanza as bytes.
	var ctrlTarGz bytes.Buffer
	gzw := gzip.NewWriter(&ctrlTarGz)
	tw := tar.NewWriter(gzw)
	body := []byte(controlStanza)
	if err := tw.WriteHeader(&tar.Header{
		Name: "./control",
		Mode: 0o644,
		Size: int64(len(body)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}

	// 2. trivial data.tar.gz (empty tarball)
	var dataTarGz bytes.Buffer
	gzw = gzip.NewWriter(&dataTarGz)
	tw = tar.NewWriter(gzw)
	tw.Close()
	gzw.Close()

	// 3. Wrap in ar archive
	var ar bytes.Buffer
	ar.WriteString("!<arch>\n")
	writeARMember(&ar, "debian-binary", []byte("2.0\n"))
	writeARMember(&ar, "control.tar.gz", ctrlTarGz.Bytes())
	writeARMember(&ar, "data.tar.gz", dataTarGz.Bytes())

	return ar.Bytes()
}

func writeARMember(w *bytes.Buffer, name string, body []byte) {
	hdr := make([]byte, 60)
	for i := range hdr {
		hdr[i] = ' '
	}
	copy(hdr[0:16], []byte(name))
	// trailer
	hdr[58] = 0x60
	hdr[59] = 0x0a
	// size at bytes 48-58
	sizeStr := strconv.Itoa(len(body))
	copy(hdr[48:48+len(sizeStr)], []byte(sizeStr))
	w.Write(hdr)
	w.Write(body)
	if len(body)%2 != 0 {
		w.WriteByte('\n')
	}
}

func TestParseAR_FakeDeb(t *testing.T) {
	deb := buildFakeDeb(t, "Package: foo\nVersion: 1.0\nArchitecture: amd64\n")
	if err := MustHaveARMagic(deb); err != nil {
		t.Fatal(err)
	}
	members, err := ParseARHeaders(deb)
	if err != nil {
		t.Fatalf("ParseARHeaders: %v", err)
	}
	if len(members) != 3 {
		t.Fatalf("got %d members, want 3", len(members))
	}
	if members[0].Name != "debian-binary" {
		t.Fatalf("first member = %q", members[0].Name)
	}
	ctrl, err := FindControlMember(members)
	if err != nil {
		t.Fatal(err)
	}
	if ctrl.Name != "control.tar.gz" {
		t.Fatalf("control name = %q", ctrl.Name)
	}
}

func TestExtractControl_FakeDeb(t *testing.T) {
	stanza := "Package: foo\nVersion: 1.0\nArchitecture: amd64\nDescription: test\n"
	deb := buildFakeDeb(t, stanza)
	members, _ := ParseARHeaders(deb)
	ctrl, _ := FindControlMember(members)
	body, _ := SliceMember(deb, ctrl)
	got, err := ExtractControl(ctrl.Name, body)
	if err != nil {
		t.Fatalf("ExtractControl: %v", err)
	}
	if string(got) != stanza {
		t.Fatalf("got %q want %q", got, stanza)
	}
}

func TestFetcher_RangeFetchRoundtrip(t *testing.T) {
	stanza := "Package: bar\nVersion: 1.2.3\nArchitecture: arm64\n"
	deb := buildFakeDeb(t, stanza)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(t, w, r, deb)
	}))
	defer srv.Close()

	f := New(srv.Client())
	res, err := f.Fetch(context.Background(), Options{URL: srv.URL + "/foo.deb"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !strings.Contains(string(res.Control), "Package: bar") {
		t.Fatalf("control missing Package: %q", res.Control)
	}
	if res.Size != int64(len(deb)) {
		t.Fatalf("size = %d, want %d", res.Size, len(deb))
	}
}

func TestFetcher_NoRangeSupportFallback(t *testing.T) {
	stanza := "Package: baz\nVersion: 0.0.1\nArchitecture: all\n"
	deb := buildFakeDeb(t, stanza)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Always 200, ignore Range
		w.Header().Set("Content-Length", strconv.Itoa(len(deb)))
		w.WriteHeader(200)
		w.Write(deb)
	}))
	defer srv.Close()

	f := New(srv.Client())
	res, err := f.Fetch(context.Background(), Options{URL: srv.URL + "/foo.deb", NeedHash: true})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.SHA256 == "" {
		t.Fatal("expected SHA256 to be populated when server returns 200")
	}
	want := sha256.Sum256(deb)
	if res.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("hash mismatch: got %s want %x", res.SHA256, want)
	}
	if res.Size != int64(len(deb)) {
		t.Fatalf("size = %d want %d", res.Size, len(deb))
	}
}

func TestFetcher_NeedHashWith206TriggersStream(t *testing.T) {
	stanza := "Package: q\nVersion: 1\nArchitecture: amd64\n"
	deb := buildFakeDeb(t, stanza)

	var fullGets, rangeGets int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			rangeGets++
			serveRange(t, w, r, deb)
			return
		}
		fullGets++
		w.WriteHeader(200)
		w.Write(deb)
	}))
	defer srv.Close()

	f := New(srv.Client())
	res, err := f.Fetch(context.Background(), Options{URL: srv.URL + "/foo.deb", NeedHash: true})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.SHA256 == "" {
		t.Fatal("expected SHA256 populated")
	}
	if rangeGets != 1 || fullGets != 1 {
		t.Fatalf("expected 1 range + 1 full GET, got %d range, %d full", rangeGets, fullGets)
	}
	want := sha256.Sum256(deb)
	if res.SHA256 != hex.EncodeToString(want[:]) {
		t.Fatalf("hash mismatch")
	}
}

// TestStreamHash_SlowButProgressingSucceeds proves the drain is bounded by
// progress, not total time: chunks arrive with gaps shorter than StallTimeout
// but a total transfer time well past it, and the hash still completes. This
// is the zoom-amd64 case — a large asset over a slow link.
func TestStreamHash_SlowButProgressingSucceeds(t *testing.T) {
	body := bytes.Repeat([]byte("z"), 4096)
	const chunk = 256
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("ResponseWriter is not a Flusher")
		}
		w.WriteHeader(200)
		for i := 0; i < len(body); i += chunk {
			w.Write(body[i : i+chunk])
			fl.Flush()
			time.Sleep(15 * time.Millisecond) // per-gap < StallTimeout
		}
	}))
	defer srv.Close()

	f := New(srv.Client())
	f.StallTimeout = 200 * time.Millisecond // 16 gaps * 15ms ~= 240ms total > StallTimeout

	hash, n, err := f.streamHash(context.Background(), Options{URL: srv.URL})
	if err != nil {
		t.Fatalf("streamHash on slow-but-live link: %v", err)
	}
	if n != int64(len(body)) {
		t.Fatalf("drained %d bytes, want %d", n, len(body))
	}
	want := sha256.Sum256(body)
	if hash != hex.EncodeToString(want[:]) {
		t.Fatalf("hash mismatch")
	}
}

// TestStreamHash_StallAborts proves a connection that accepts then goes silent
// past StallTimeout is aborted with a clear no-progress error rather than
// hanging until some size-proportional total cap.
func TestStreamHash_StallAborts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		w.WriteHeader(200)
		w.Write([]byte("partial"))
		fl.Flush()
		time.Sleep(400 * time.Millisecond) // >> StallTimeout: no further progress
		w.Write([]byte("never-read"))
	}))
	defer srv.Close()

	f := New(srv.Client())
	f.StallTimeout = 100 * time.Millisecond

	start := time.Now()
	_, _, err := f.streamHash(context.Background(), Options{URL: srv.URL})
	if err == nil {
		t.Fatal("expected stall error, got nil")
	}
	if !strings.Contains(err.Error(), "no progress") {
		t.Fatalf("error = %q, want it to mention no progress", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("aborted after %s; stall watchdog should have fired near 100ms", elapsed)
	}
}

// serveRange honors a single bytes=start-end range header, returning 206.
// If no Range header, returns 200 with the full body. Mimics S3-style
// servers (GitHub asset storage).
func serveRange(t *testing.T, w http.ResponseWriter, r *http.Request, body []byte) {
	t.Helper()
	rng := r.Header.Get("Range")
	if rng == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		w.Write(body)
		return
	}
	if !strings.HasPrefix(rng, "bytes=") {
		http.Error(w, "bad range", 416)
		return
	}
	parts := strings.SplitN(strings.TrimPrefix(rng, "bytes="), "-", 2)
	start, _ := strconv.ParseInt(parts[0], 10, 64)
	end := int64(len(body) - 1)
	if parts[1] != "" {
		v, _ := strconv.ParseInt(parts[1], 10, 64)
		if v < end {
			end = v
		}
	}
	if start > end {
		http.Error(w, "bad range", 416)
		return
	}
	chunk := body[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	w.WriteHeader(206)
	w.Write(chunk)
}

// TestFetcher_ControlExceedsHeadWindow synthesizes a .deb where control.tar.gz
// is larger than HeadWindow, forcing the second-range path.
func TestFetcher_ControlExceedsHeadWindow(t *testing.T) {
	// Build a control stanza with a long Description so control.tar.gz
	// (post-compression) exceeds, say, 4KB. Then we'll set HeadWindow tiny.
	// Easier path: tweak rangeFetch to use a small head, but that's package-
	// internal. So instead, just trust the existing range-tail logic and
	// provide a smaller-window Fetcher via a custom client wrapping the
	// transport. Easiest: write a unit test against extractControl directly.
	stanza := "Package: big\nVersion: 1.0\nArchitecture: amd64\nDescription: " +
		strings.Repeat("x", 100) + "\n"
	deb := buildFakeDeb(t, stanza)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(t, w, r, deb)
	}))
	defer srv.Close()

	// Simulate "head too small" by feeding extractControl a deliberately
	// truncated head and asserting the range-tail path completes.
	f := New(srv.Client())
	// Find the control member in the full deb to know its real offset/size.
	members, _ := ParseARHeaders(deb)
	ctrl, _ := FindControlMember(members)
	// Truncate head before control ends.
	headLen := ctrl.Offset + ctrl.Size/2
	if headLen <= ctrl.Offset {
		t.Skip("control too small to truncate meaningfully")
	}
	head := append([]byte(nil), deb[:headLen]...)
	got, err := f.extractControl(context.Background(), Options{URL: srv.URL + "/foo.deb"}, head, int64(len(deb)))
	if err != nil {
		t.Fatalf("extractControl with truncated head: %v", err)
	}
	if !strings.Contains(string(got), "Package: big") {
		t.Fatalf("control body missing Package")
	}
	_ = io.Discard
}
