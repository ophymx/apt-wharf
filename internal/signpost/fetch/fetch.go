// Package fetch issues HTTP GETs against upstream .deb URLs, parses the ar
// archive in the response prefix, and extracts the inner control stanza.
//
// Two code paths:
//
//   - Range-fetch (default): one GET with Range: bytes=0-1048575. On 206
//     we parse ar from the buffer; if control.tar exceeds the buffer we
//     issue a precise second range. If the server returns 200 (no range
//     support), we have the full body already and use it directly.
//
//   - Streaming (when caller passes NeedHash=true and an initial range
//     attempt got 206 — i.e. we still need to drain the asset to compute
//     SHA256): a follow-up plain GET that streams the body through a
//     hasher.
package fetch

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// HeadWindow is the size of the initial range request. Comfortably
	// covers Debian control archives which top out around 50 KiB.
	HeadWindow = 1 << 20

	// MaxControlTar caps the buffer we'll allocate for a control.tar
	// member that doesn't fit in HeadWindow. 16 MiB is absurdly generous;
	// in practice we never see >1 MiB.
	MaxControlTar = 16 << 20

	// DefaultStallTimeout bounds the hash-pass drain by *progress*, not by
	// total size: the download is aborted only when no bytes arrive for this
	// long. A healthy transfer of any size survives; a dead connection dies
	// promptly.
	DefaultStallTimeout = 60 * time.Second

	// DefaultResponseHeaderTimeout caps the wait for the hash-pass server to
	// start responding (headers), independent of body size.
	DefaultResponseHeaderTimeout = 30 * time.Second
)

// errStall is the cancel cause the read watchdog attaches when the hash-pass
// body makes no progress for StallTimeout.
var errStall = errors.New("read stalled")

// Options configure a single fetch.
type Options struct {
	URL      string
	Headers  http.Header // copied verbatim onto every request (Authorization etc.)
	NeedHash bool        // when true, ensure result.SHA256 is populated
}

// Result captures everything a refresh tick needs from one .deb fetch.
type Result struct {
	Control []byte // raw control file bytes (no trailing checksums; caller appends)
	Size    int64  // total .deb size
	SHA256  string // lowercase hex; empty when !Options.NeedHash and we never streamed
}

// Fetcher performs control-stanza extraction with optional whole-asset hashing.
type Fetcher struct {
	// Client serves probes and the small (<=1 MiB) control-range fetches. Its
	// http.Client.Timeout is a fine total cap for those bounded exchanges.
	Client *http.Client

	// HashClient streams the full asset for the SHA256 pass. It must NOT set a
	// total http.Client.Timeout — that cap is size-proportional and any large
	// enough .deb over any slow enough link would always trip it. Progress is
	// instead bounded by transport-level timeouts (dial/TLS/response-header)
	// plus the StallTimeout read watchdog. Nil falls back to Client.
	HashClient *http.Client

	// StallTimeout is the no-progress window for the hash-pass drain. <=0
	// selects DefaultStallTimeout.
	StallTimeout time.Duration
}

// New builds a Fetcher whose hash-pass client is derived from client's
// transport but strips the total timeout, so full-asset drains are bounded by
// progress rather than size. client still serves probes and control-range
// fetches under its own Timeout.
func New(client *http.Client) *Fetcher {
	return &Fetcher{
		Client:       client,
		HashClient:   newHashClient(client),
		StallTimeout: DefaultStallTimeout,
	}
}

// newHashClient returns a client with no total Timeout, reusing base's
// transport (proxy/TLS/dial settings) but ensuring a response-header timeout
// so a server that accepts the connection yet never replies still fails fast.
func newHashClient(base *http.Client) *http.Client {
	var rt http.RoundTripper
	switch {
	case base != nil && base.Transport != nil:
		if t, ok := base.Transport.(*http.Transport); ok {
			c := t.Clone()
			if c.ResponseHeaderTimeout == 0 {
				c.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
			}
			rt = c
		} else {
			// Custom RoundTripper (e.g. a test double) — reuse verbatim.
			rt = base.Transport
		}
	default:
		c := http.DefaultTransport.(*http.Transport).Clone()
		c.ResponseHeaderTimeout = DefaultResponseHeaderTimeout
		rt = c
	}
	return &http.Client{Transport: rt}
}

// Fetch performs the full extraction pipeline for a single .deb URL.
func (f *Fetcher) Fetch(ctx context.Context, opts Options) (*Result, error) {
	res, err := f.rangeFetch(ctx, opts)
	if err != nil {
		return nil, err
	}
	if !opts.NeedHash || res.SHA256 != "" {
		return res, nil
	}
	// We got control via 206 and caller wants a hash → drain the asset.
	hash, size, err := f.streamHash(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("hash pass: %w", err)
	}
	if size != res.Size && res.Size > 0 {
		return nil, fmt.Errorf("size mismatch between range (%d) and full GET (%d)", res.Size, size)
	}
	res.SHA256 = hash
	if res.Size == 0 {
		res.Size = size
	}
	return res, nil
}

// rangeFetch returns res with Control + Size populated. SHA256 is populated
// only when the server returned 200 and we hashed the body in-stream; on
// 206 the caller is responsible for a follow-up streaming pass.
func (f *Fetcher) rangeFetch(ctx context.Context, opts Options) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, opts.Headers)
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", HeadWindow-1))

	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", opts.URL, err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusPartialContent:
		head, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read head: %w", err)
		}
		size, err := totalFromContentRange(resp.Header.Get("Content-Range"))
		if err != nil {
			return nil, err
		}
		ctrl, err := f.extractControl(ctx, opts, head, size)
		if err != nil {
			return nil, err
		}
		return &Result{Control: ctrl, Size: size}, nil

	case http.StatusOK:
		// Server doesn't support range — we got the full body. Hash inline
		// regardless of NeedHash; the cost is the same and it short-circuits
		// the streaming pass.
		h := sha256.New()
		var buf bytes.Buffer
		n, err := io.Copy(io.MultiWriter(&buf, h), resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read body: %w", err)
		}
		ctrl, err := f.extractControl(ctx, opts, buf.Bytes(), n)
		if err != nil {
			return nil, err
		}
		return &Result{
			Control: ctrl,
			Size:    n,
			SHA256:  hex.EncodeToString(h.Sum(nil)),
		}, nil

	default:
		return nil, fmt.Errorf("GET %s: unexpected status %s", opts.URL, resp.Status)
	}
}

// extractControl parses the ar archive in head, locates the control member,
// and returns the inner control file bytes. If the control member extends
// past head, fetches the missing tail with a precise second range request.
//
// totalSize is used only to detect malformed cases (control claims to
// extend past EOF).
func (f *Fetcher) extractControl(ctx context.Context, opts Options, head []byte, totalSize int64) ([]byte, error) {
	if err := MustHaveARMagic(head); err != nil {
		return nil, err
	}
	members, err := ParseARHeaders(head)
	if err != nil {
		return nil, err
	}
	ctrl, err := FindControlMember(members)
	if err != nil {
		return nil, err
	}
	if ctrl.Size > MaxControlTar {
		return nil, fmt.Errorf("control.tar size %d exceeds cap %d", ctrl.Size, MaxControlTar)
	}
	if totalSize > 0 && ctrl.Offset+ctrl.Size > totalSize {
		return nil, fmt.Errorf("control.tar (offset=%d size=%d) extends past asset end (%d)",
			ctrl.Offset, ctrl.Size, totalSize)
	}

	var ctrlTar []byte
	if buf, ok := SliceMember(head, ctrl); ok {
		ctrlTar = buf
	} else {
		have := head[ctrl.Offset:]
		need := ctrl.Size - int64(len(have))
		tailStart := ctrl.Offset + int64(len(have))
		tailEnd := ctrl.Offset + ctrl.Size - 1
		tail, err := f.rangeBody(ctx, opts.URL, opts.Headers, tailStart, tailEnd)
		if err != nil {
			return nil, fmt.Errorf("range tail [%d-%d]: %w", tailStart, tailEnd, err)
		}
		if int64(len(tail)) != need {
			return nil, fmt.Errorf("range tail returned %d bytes, expected %d",
				len(tail), need)
		}
		ctrlTar = make([]byte, ctrl.Size)
		copy(ctrlTar, have)
		copy(ctrlTar[len(have):], tail)
	}

	control, err := ExtractControl(ctrl.Name, ctrlTar)
	if err != nil {
		return nil, fmt.Errorf("extract control: %w", err)
	}
	return control, nil
}

// rangeBody issues GET with a precise Range header and returns the body.
func (f *Fetcher) rangeBody(ctx context.Context, url string, headers http.Header, start, end int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	applyHeaders(req, headers)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, fmt.Errorf("range GET returned %s, expected 206", resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// streamHash performs a full GET, computes SHA256, returns hash + size. No
// Range header — server may return 200 or 206; either is fine, we drain
// everything. The drain is bounded by progress, not by total size: a read
// watchdog cancels the request if no bytes arrive for StallTimeout, so a large
// asset over a slow-but-live link succeeds while a stalled one fails promptly.
func (f *Fetcher) streamHash(ctx context.Context, opts Options) (string, int64, error) {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
	if err != nil {
		return "", 0, err
	}
	applyHeaders(req, opts.Headers)

	client := f.HashClient
	if client == nil {
		client = f.Client
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("GET %s: unexpected status %s", opts.URL, resp.Status)
	}

	d := f.StallTimeout
	if d <= 0 {
		d = DefaultStallTimeout
	}
	sr := &stallReader{rc: resp.Body, d: d}
	sr.timer = time.AfterFunc(d, func() { cancel(errStall) })
	defer sr.timer.Stop()

	h := sha256.New()
	n, err := io.Copy(h, sr)
	if err != nil {
		if errors.Is(context.Cause(ctx), errStall) {
			return "", 0, fmt.Errorf("drain body: no progress for %s", d)
		}
		return "", 0, fmt.Errorf("drain body: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

// stallReader wraps a response body and rearms a watchdog on every read that
// makes progress. When the watchdog fires it cancels the request context,
// unblocking an in-flight Read with the errStall cause.
type stallReader struct {
	rc    io.Reader
	d     time.Duration
	timer *time.Timer
}

func (s *stallReader) Read(p []byte) (int, error) {
	n, err := s.rc.Read(p)
	if n > 0 {
		s.timer.Reset(s.d)
	}
	return n, err
}

func applyHeaders(req *http.Request, h http.Header) {
	for k, vs := range h {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}

// totalFromContentRange parses "bytes 0-1048575/12345678" → 12345678.
func totalFromContentRange(v string) (int64, error) {
	if v == "" {
		return 0, fmt.Errorf("missing Content-Range header")
	}
	slash := strings.LastIndex(v, "/")
	if slash < 0 {
		return 0, fmt.Errorf("malformed Content-Range %q", v)
	}
	tail := v[slash+1:]
	if tail == "*" {
		return 0, nil // server doesn't know total; tolerated
	}
	n, err := strconv.ParseInt(tail, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("Content-Range %q total: %w", v, err)
	}
	return n, nil
}
