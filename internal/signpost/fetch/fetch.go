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
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

const (
	// HeadWindow is the size of the initial range request. Comfortably
	// covers Debian control archives which top out around 50 KiB.
	HeadWindow = 1 << 20

	// MaxControlTar caps the buffer we'll allocate for a control.tar
	// member that doesn't fit in HeadWindow. 16 MiB is absurdly generous;
	// in practice we never see >1 MiB.
	MaxControlTar = 16 << 20
)

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
	Client *http.Client
}

func New(client *http.Client) *Fetcher { return &Fetcher{Client: client} }

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
// everything.
func (f *Fetcher) streamHash(ctx context.Context, opts Options) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, opts.URL, nil)
	if err != nil {
		return "", 0, err
	}
	applyHeaders(req, opts.Headers)
	resp, err := f.Client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", 0, fmt.Errorf("GET %s: unexpected status %s", opts.URL, resp.Status)
	}
	h := sha256.New()
	n, err := io.Copy(h, resp.Body)
	if err != nil {
		return "", 0, fmt.Errorf("drain body: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
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
