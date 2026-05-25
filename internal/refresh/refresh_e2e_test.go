package refresh

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	openpgpClearsign "github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"

	"github.com/ophymx/apt-wharf/internal/config"
	"github.com/ophymx/apt-wharf/internal/fetch"
	"github.com/ophymx/apt-wharf/internal/sign"
	"github.com/ophymx/apt-wharf/internal/source"
	"github.com/ophymx/apt-wharf/internal/store"
)

// TestRefresh_EndToEnd wires every package together against a synthetic
// GitHub API + asset host. Validates that one full refresh tick produces
// signed metadata, by-hash entries, pool redirects, and a bootstrap .deb.
func TestRefresh_EndToEnd(t *testing.T) {
	// 1. Build a fake .deb for source "widget"; serve it from an httptest server.
	debBytes := buildFakeDeb(t, "widget", "1.2.3", "amd64")
	debSHA := sha256.Sum256(debBytes)
	debSHAHex := hex.EncodeToString(debSHA[:])

	assetSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRange(t, w, r, debBytes)
	}))
	defer assetSrv.Close()

	// 2. Mock GitHub API.
	apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/releases/latest") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("ETag", `W/"r1"`)
		json.NewEncoder(w).Encode(map[string]any{
			"id":       1,
			"tag_name": "v1.2.3",
			"assets": []map[string]any{
				{
					"name":                 "widget_1.2.3_amd64.deb",
					"size":                 len(debBytes),
					"digest":               "sha256:" + debSHAHex,
					"browser_download_url": assetSrv.URL + "/widget_1.2.3_amd64.deb",
				},
			},
		})
	}))
	defer apiSrv.Close()

	// 3. Generate a real signing key; write to a tmp path.
	tmp := t.TempDir()
	keyPath := filepath.Join(tmp, "secring.gpg")
	signingEntity := newPGPEntity(t, "Test Signer")
	writePGPSecret(t, keyPath, signingEntity)

	// 4. Build a Wired-equivalent by hand (avoiding cmd/signpost.Wire so we
	//    can override discoverer BaseURL after construction).
	signer, err := sign.Load(keyPath, nil, "")
	if err != nil {
		t.Fatalf("sign.Load: %v", err)
	}

	stateDir := filepath.Join(tmp, "state")
	st := store.New(stateDir)
	if err := st.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	httpClient := &http.Client{Timeout: 30 * time.Second}
	fetcher := fetch.New(httpClient)

	cfg := &config.Config{
		Repository: config.Repository{
			Origin: "Acme", Label: "Acme APT",
			BaseURL: "https://apt.acme.example",
		},
		Suite: config.Suite{
			Codename: "stable", Description: "Acme stable",
			Architectures: []string{"amd64", "arm64"},
		},
		Bootstrap: config.Bootstrap{
			PackageName: "acme-archive-keyring",
			Maintainer:  "Acme Ops <ops@acme.example>",
			Description: "Acme APT keyring",
		},
		Refresh: config.Refresh{
			Interval:    config.Duration(time.Hour),
			Jitter:      0,
			HTTPTimeout: config.Duration(30 * time.Second),
		},
		GitHub: config.GitHub{
			RateLimit: config.RateLimit{
				UnauthenticatedPerHour: 50,
				AuthenticatedPerHour:   4500,
			},
		},
		Paths: config.Paths{StateDir: stateDir},
		Sources: map[string]*config.Source{
			"vendor-widget-amd64": {
				Discovery: config.Discovery{
					Type:  "github_release",
					Repo:  "acme/widget",
					Asset: `widget_.*_amd64\.deb`,
				},
			},
		},
	}

	registry := source.NewRegistry(50, 4500)
	ghClient := source.NewGitHubClient(httpClient, nil)
	apiBase, _ := url.Parse(apiSrv.URL + "/")
	ghClient.BaseURL = apiBase
	disc, err := source.NewGitHubReleaseDiscoverer("acme/widget", `widget_.*_amd64\.deb`, false,
		registry.BucketFor(nil), registry.CredentialID(nil), ghClient)
	if err != nil {
		t.Fatalf("NewGitHubReleaseDiscoverer: %v", err)
	}

	holder := &Holder{}
	rf := New(Options{
		Cfg:         cfg,
		Signer:      signer,
		Store:       st,
		Fetcher:     fetcher,
		HTTPClient:  httpClient,
		Discoverers: map[string]source.Discoverer{"vendor-widget-amd64": disc},
		Holder:      holder,
		Logger:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := rf.SyncImportNew(ctx); err != nil {
		t.Fatalf("SyncImportNew: %v", err)
	}
	if err := rf.Refresh(ctx); err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	snap := holder.Load()
	if snap == nil {
		t.Fatal("snapshot not stored")
	}

	// Assertions:
	t.Run("InRelease present and signed", func(t *testing.T) {
		f, ok := snap.Files["/dists/stable/InRelease"]
		if !ok {
			t.Fatal("InRelease missing")
		}
		if !bytes.HasPrefix(f.Data, []byte("-----BEGIN PGP SIGNED MESSAGE-----")) {
			t.Fatalf("InRelease not clearsigned, head=%q", f.Data[:64])
		}
		// Verify clearsign + signature.
		block, _ := openpgpClearsign.Decode(f.Data)
		if block == nil {
			t.Fatal("clearsign.Decode returned nil")
		}
		_, err := openpgp.CheckDetachedSignature(openpgp.EntityList{signingEntity},
			bytes.NewReader(block.Bytes), block.ArmoredSignature.Body, nil)
		if err != nil {
			t.Fatalf("InRelease signature does not verify: %v", err)
		}
	})

	t.Run("Release.gpg present and verifies", func(t *testing.T) {
		rel := snap.Files["/dists/stable/Release"].Data
		sig := snap.Files["/dists/stable/Release.gpg"].Data
		if len(rel) == 0 || len(sig) == 0 {
			t.Fatal("Release or Release.gpg missing")
		}
		if _, err := openpgp.CheckArmoredDetachedSignature(openpgp.EntityList{signingEntity},
			bytes.NewReader(rel), bytes.NewReader(sig), nil); err != nil {
			t.Fatalf("Release.gpg verify: %v", err)
		}
	})

	t.Run("per-arch Packages contains widget and bootstrap", func(t *testing.T) {
		f := snap.Files["/dists/stable/main/binary-amd64/Packages"]
		if !bytes.Contains(f.Data, []byte("Package: widget")) {
			t.Errorf("amd64 Packages missing widget, got:\n%s", f.Data)
		}
		if !bytes.Contains(f.Data, []byte("Package: acme-archive-keyring")) {
			t.Errorf("amd64 Packages missing bootstrap stanza")
		}
		// arm64 Packages should NOT contain widget (amd64-only) but SHOULD contain bootstrap.
		fa := snap.Files["/dists/stable/main/binary-arm64/Packages"]
		if bytes.Contains(fa.Data, []byte("Package: widget")) {
			t.Errorf("arm64 Packages incorrectly contains amd64-only widget")
		}
		if !bytes.Contains(fa.Data, []byte("Package: acme-archive-keyring")) {
			t.Errorf("arm64 Packages missing bootstrap stanza (Architecture: all should fan in)")
		}
	})

	t.Run("by-hash path exists with matching content", func(t *testing.T) {
		canonical := snap.Files["/dists/stable/main/binary-amd64/Packages"]
		sum := sha256.Sum256(canonical.Data)
		hexSum := hex.EncodeToString(sum[:])
		byHash := snap.Files["/dists/stable/main/binary-amd64/by-hash/SHA256/"+hexSum]
		if !bytes.Equal(byHash.Data, canonical.Data) {
			t.Error("by-hash Packages bytes do not match canonical")
		}
	})

	t.Run("pool redirect for widget points to upstream", func(t *testing.T) {
		want := assetSrv.URL + "/widget_1.2.3_amd64.deb"
		rd := snap.Redirects["/pool/main/w/widget/widget_1.2.3_amd64.deb"]
		if rd.URL != want {
			t.Fatalf("redirect URL = %q want %q", rd.URL, want)
		}
	})

	t.Run("bootstrap deb served at pool path", func(t *testing.T) {
		// Find the bootstrap pool path by scanning snapshot for any /pool/.../acme-archive-keyring_*_all.deb
		var found bool
		for path, f := range snap.Files {
			if strings.HasPrefix(path, "/pool/main/a/acme-archive-keyring/") &&
				strings.HasSuffix(path, "_all.deb") {
				if !bytes.HasPrefix(f.Data, []byte("!<arch>\n")) {
					t.Fatal("bootstrap deb does not start with ar magic")
				}
				found = true
			}
		}
		if !found {
			t.Fatal("bootstrap deb pool path not found in snapshot")
		}
	})

	t.Run("/release/stable/latest redirect + version text", func(t *testing.T) {
		latest, ok := snap.Files["/release/stable/latest"]
		if !ok || len(latest.Data) == 0 {
			t.Fatal("/release/stable/latest missing")
		}
		rd, ok := snap.Redirects["/release/stable/latest.deb"]
		if !ok || !strings.HasPrefix(rd.URL, "/pool/main/a/acme-archive-keyring/") {
			t.Fatalf("/release/stable/latest.deb redirect = %+v", rd)
		}
	})

	t.Run("/pubkey.gpg matches signer keyring", func(t *testing.T) {
		got := snap.Files["/pubkey.gpg"].Data
		if !bytes.Equal(got, signer.KeyringBytes()) {
			t.Error("/pubkey.gpg differs from signer KeyringBytes")
		}
	})

	t.Run("state file persisted with parsed control", func(t *testing.T) {
		states, err := st.LoadSources()
		if err != nil {
			t.Fatal(err)
		}
		s := states["vendor-widget-amd64"]
		if s == nil {
			t.Fatal("source state missing")
		}
		if s.AssetSHA256 != debSHAHex {
			t.Fatalf("state SHA256 = %s want %s", s.AssetSHA256, debSHAHex)
		}
		if !strings.Contains(s.Control, "Package: widget") {
			t.Fatalf("state control missing Package, got %q", s.Control)
		}
	})

	t.Run("second refresh tick is a no-op (304 path)", func(t *testing.T) {
		// Mock currently always returns 200; rewrap to honor If-None-Match.
		var apiCalls int
		apiSrv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiCalls++
			if r.Header.Get("If-None-Match") == `W/"r1"` {
				w.WriteHeader(http.StatusNotModified)
				return
			}
			w.Header().Set("ETag", `W/"r1"`)
			json.NewEncoder(w).Encode(map[string]any{
				"id":       1,
				"tag_name": "v1.2.3",
				"assets": []map[string]any{{
					"name":                 "widget_1.2.3_amd64.deb",
					"size":                 len(debBytes),
					"digest":               "sha256:" + debSHAHex,
					"browser_download_url": assetSrv.URL + "/widget_1.2.3_amd64.deb",
				}},
			})
		}))
		defer apiSrv2.Close()
		newBase, _ := url.Parse(apiSrv2.URL + "/")
		ghClient.BaseURL = newBase

		if err := rf.Refresh(ctx); err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if apiCalls != 1 {
			t.Fatalf("expected 1 API call (only /releases/latest), got %d", apiCalls)
		}
	})

	_ = os.Stderr // silence unused import
}

// --- helpers ---

func buildFakeDeb(t *testing.T, pkg, version, arch string) []byte {
	t.Helper()
	stanza := fmt.Sprintf("Package: %s\nVersion: %s\nArchitecture: %s\nMaintainer: Test <t@t>\nDescription: t\n",
		pkg, version, arch)

	var ctrlTarGz bytes.Buffer
	gzw := gzip.NewWriter(&ctrlTarGz)
	tw := tar.NewWriter(gzw)
	body := []byte(stanza)
	tw.WriteHeader(&tar.Header{Name: "./control", Mode: 0o644, Size: int64(len(body))})
	tw.Write(body)
	tw.Close()
	gzw.Close()

	var dataTarGz bytes.Buffer
	gzw = gzip.NewWriter(&dataTarGz)
	tw = tar.NewWriter(gzw)
	tw.Close()
	gzw.Close()

	var ar bytes.Buffer
	ar.WriteString("!<arch>\n")
	writeAR(&ar, "debian-binary", []byte("2.0\n"))
	writeAR(&ar, "control.tar.gz", ctrlTarGz.Bytes())
	writeAR(&ar, "data.tar.gz", dataTarGz.Bytes())
	return ar.Bytes()
}

func writeAR(w *bytes.Buffer, name string, body []byte) {
	hdr := bytes.Repeat([]byte{' '}, 60)
	copy(hdr[0:16], []byte(name))
	hdr[58] = 0x60
	hdr[59] = 0x0a
	sz := strconv.Itoa(len(body))
	copy(hdr[48:48+len(sz)], []byte(sz))
	w.Write(hdr)
	w.Write(body)
	if len(body)%2 != 0 {
		w.WriteByte('\n')
	}
}

func serveRange(t *testing.T, w http.ResponseWriter, r *http.Request, body []byte) {
	t.Helper()
	rng := r.Header.Get("Range")
	if rng == "" {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(200)
		w.Write(body)
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
	chunk := body[start : end+1]
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(body)))
	w.Header().Set("Content-Length", strconv.Itoa(len(chunk)))
	w.WriteHeader(206)
	w.Write(chunk)
}

func newPGPEntity(t *testing.T, name string) *openpgp.Entity {
	t.Helper()
	e, err := openpgp.NewEntity(name, "test", "test@example", &packet.Config{RSABits: 2048})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func writePGPSecret(t *testing.T, path string, e *openpgp.Entity) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := e.SerializePrivate(f, &packet.Config{}); err != nil {
		t.Fatal(err)
	}
}
