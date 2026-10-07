package export

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ophymx/apt-wharf/internal/signpost/refresh"
)

func testSnapshot() *refresh.Snapshot {
	lm := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	return &refresh.Snapshot{
		BuiltAt: time.Date(2026, 10, 7, 9, 30, 0, 0, time.UTC),
		Files: map[string]refresh.FileEntry{
			"/dists/stable/InRelease":                                                     {Data: []byte("inrelease"), ContentType: "text/plain", LastModified: lm},
			"/dists/stable/Release":                                                       {Data: []byte("release"), ContentType: "text/plain", LastModified: lm},
			"/dists/stable/Release.gpg":                                                   {Data: []byte("sig"), ContentType: "application/pgp-signature", LastModified: lm},
			"/dists/stable/main/binary-amd64/Packages":                                    {Data: []byte("pkgs"), ContentType: "text/plain", LastModified: lm},
			"/dists/stable/main/binary-amd64/Packages.gz":                                 {Data: []byte("gz"), ContentType: "application/gzip", LastModified: lm},
			"/dists/stable/main/binary-amd64/by-hash/SHA256/aaaa":                         {Data: []byte("pkgs"), ContentType: "text/plain"},
			"/pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.10.01.1_all.deb": {Data: []byte("deb"), ContentType: "application/vnd.debian.binary-package"},
			"/pubkey.gpg":            {Data: []byte("key"), ContentType: "application/pgp-keys"},
			"/release/stable/latest": {Data: []byte("2026.10.01.1\n"), ContentType: "text/plain"},
		},
		Redirects: map[string]refresh.Redirect{
			"/pool/main/w/widget/widget_1.2.3_amd64.deb": {URL: "https://github.com/acme/widget/releases/download/v1.2.3/widget_1.2.3_amd64.deb"},
			"/pool/main/b/bolt/bolt_0.9_arm64.deb":       {URL: "https://cdn.example.com/bolt_0.9_arm64.deb"},
			"/release/stable/latest.deb":                 {URL: "/apt/pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.10.01.1_all.deb"},
		},
	}
}

func allTargets(t *testing.T) []Target {
	t.Helper()
	ts, err := ParseTargets([]string{"cloudflare,nginx", "manifest"})
	if err != nil {
		t.Fatal(err)
	}
	return ts
}

func readOut(t *testing.T, dir, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func TestWrite_LayoutAndTargets(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site")
	snap := testSnapshot()
	res, err := Write(snap, Options{OutDir: dir, Targets: allTargets(t)})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	wantWritten := len(snap.Files) + 4 // _redirects, _headers, nginx conf, manifest
	if res.Written != wantWritten || res.Pruned != 0 {
		t.Fatalf("result = %+v, want written=%d pruned=0", res, wantWritten)
	}

	// Site tree.
	if got := readOut(t, dir, "dists/stable/InRelease"); got != "inrelease" {
		t.Errorf("InRelease = %q", got)
	}
	if got := readOut(t, dir, "pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.10.01.1_all.deb"); got != "deb" {
		t.Errorf("bootstrap deb = %q", got)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, "**", ".tmp-export-*")); len(matches) != 0 {
		t.Errorf("temp files leaked: %v", matches)
	}

	// Cloudflare: sorted static rules, no splats, trailing 302.
	rd := readOut(t, dir, "_redirects")
	wantRedirects := strings.Join([]string{
		"/pool/main/b/bolt/bolt_0.9_arm64.deb https://cdn.example.com/bolt_0.9_arm64.deb 302",
		"/pool/main/w/widget/widget_1.2.3_amd64.deb https://github.com/acme/widget/releases/download/v1.2.3/widget_1.2.3_amd64.deb 302",
		"/release/stable/latest.deb /apt/pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.10.01.1_all.deb 302",
	}, "\n") + "\n"
	if body := stripComments(rd); body != wantRedirects {
		t.Errorf("_redirects body:\n%s\nwant:\n%s", body, wantRedirects)
	}
	hd := readOut(t, dir, "_headers")
	if !strings.Contains(hd, "/dists/*\n  Cache-Control: no-cache") || !strings.Contains(hd, "/pool/*\n  Cache-Control: public, max-age=31536000, immutable") {
		t.Errorf("_headers missing expected rules:\n%s", hd)
	}

	// nginx: quoted map entries.
	ng := readOut(t, dir, NginxConfName)
	if !strings.Contains(ng, "map $uri $signpost_redirect {") ||
		!strings.Contains(ng, `    "/pool/main/b/bolt/bolt_0.9_arm64.deb" "https://cdn.example.com/bolt_0.9_arm64.deb";`) {
		t.Errorf("nginx conf unexpected:\n%s", ng)
	}

	// Manifest: parses, sorted, cache classes.
	var m Manifest
	if err := json.Unmarshal([]byte(readOut(t, dir, ManifestName)), &m); err != nil {
		t.Fatalf("manifest parse: %v", err)
	}
	if m.SchemaVersion != ManifestSchemaVersion || len(m.Files) != len(snap.Files) || len(m.Redirects) != 3 {
		t.Fatalf("manifest shape: version=%d files=%d redirects=%d", m.SchemaVersion, len(m.Files), len(m.Redirects))
	}
	classes := map[string]string{}
	for _, f := range m.Files {
		classes[f.Path] = f.Cache
	}
	for p, want := range map[string]string{
		"/dists/stable/InRelease":                                                     CacheMetadata,
		"/dists/stable/main/binary-amd64/by-hash/SHA256/aaaa":                         CacheImmutable,
		"/pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.10.01.1_all.deb": CacheImmutable,
		"/pubkey.gpg": CacheMetadata,
	} {
		if classes[p] != want {
			t.Errorf("cache class %s = %q, want %q", p, classes[p], want)
		}
	}
	if m.Redirects[0].Path != "/pool/main/b/bolt/bolt_0.9_arm64.deb" || m.Redirects[0].Status != 302 {
		t.Errorf("manifest redirects not sorted/302: %+v", m.Redirects[0])
	}

	// Marker lists everything (site + target files) but not itself.
	var mk marker
	if err := json.Unmarshal([]byte(readOut(t, dir, MarkerName)), &mk); err != nil {
		t.Fatalf("marker parse: %v", err)
	}
	if len(mk.Paths) != wantWritten {
		t.Errorf("marker paths = %d, want %d", len(mk.Paths), wantWritten)
	}
	for _, p := range mk.Paths {
		if p == MarkerName || strings.HasPrefix(p, "/") {
			t.Errorf("marker lists bad path %q", p)
		}
	}
}

func stripComments(s string) string {
	var out []string
	for l := range strings.SplitSeq(s, "\n") {
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n") + "\n"
}

func TestWrite_PrunesOnlyWhatItWrote(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site")
	snap := testSnapshot()
	if _, err := Write(snap, Options{OutDir: dir, Targets: allTargets(t)}); err != nil {
		t.Fatal(err)
	}
	// Operator drops an unrelated file in; export must leave it alone.
	foreign := filepath.Join(dir, "robots.txt")
	if err := os.WriteFile(foreign, []byte("User-agent: *\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Next export: by-hash rotated, bootstrap bumped, nginx target dropped.
	delete(snap.Files, "/dists/stable/main/binary-amd64/by-hash/SHA256/aaaa")
	snap.Files["/dists/stable/main/binary-amd64/by-hash/SHA256/bbbb"] = refresh.FileEntry{Data: []byte("pkgs2")}
	delete(snap.Files, "/pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.10.01.1_all.deb")
	snap.Files["/pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.10.07.1_all.deb"] = refresh.FileEntry{Data: []byte("deb2")}
	ts, _ := ParseTargets([]string{"cloudflare"})
	res, err := Write(snap, Options{OutDir: dir, Targets: ts})
	if err != nil {
		t.Fatalf("second Write: %v", err)
	}
	if res.Pruned != 4 { // old by-hash, old deb, nginx conf, manifest
		t.Errorf("pruned = %d, want 4", res.Pruned)
	}
	for _, gone := range []string{
		"dists/stable/main/binary-amd64/by-hash/SHA256/aaaa",
		"pool/main/a/acme-archive-keyring/acme-archive-keyring_2026.10.01.1_all.deb",
		NginxConfName, ManifestName,
	} {
		if _, err := os.Stat(filepath.Join(dir, filepath.FromSlash(gone))); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should have been pruned (err=%v)", gone, err)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("foreign file was removed: %v", err)
	}
	if got := readOut(t, dir, "dists/stable/main/binary-amd64/by-hash/SHA256/bbbb"); got != "pkgs2" {
		t.Errorf("new by-hash = %q", got)
	}
}

func TestWrite_PrunesEmptyParents(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site")
	snap := testSnapshot()
	ts, _ := ParseTargets([]string{"manifest"})
	if _, err := Write(snap, Options{OutDir: dir, Targets: ts}); err != nil {
		t.Fatal(err)
	}
	delete(snap.Files, "/release/stable/latest")
	if _, err := Write(snap, Options{OutDir: dir, Targets: ts}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "release")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("empty release/ dir should be removed (err=%v)", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("output root must survive: %v", err)
	}
}

func TestWrite_RefusesForeignNonEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "important.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := ParseTargets([]string{"manifest"})
	_, err := Write(testSnapshot(), Options{OutDir: dir, Targets: ts})
	if err == nil || !strings.Contains(err.Error(), "refusing to write into non-empty") {
		t.Fatalf("err = %v, want refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "important.txt")); err != nil {
		t.Errorf("foreign file touched: %v", err)
	}
}

func TestWrite_AcceptsEmptyExistingDir(t *testing.T) {
	dir := t.TempDir()
	ts, _ := ParseTargets([]string{"manifest"})
	if _, err := Write(testSnapshot(), Options{OutDir: dir, Targets: ts}); err != nil {
		t.Fatalf("Write into empty dir: %v", err)
	}
}

func TestWrite_RefusesFileAsOutDir(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	ts, _ := ParseTargets([]string{"manifest"})
	if _, err := Write(testSnapshot(), Options{OutDir: f, Targets: ts}); err == nil || !strings.Contains(err.Error(), "not a directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestWriteOrder_LeavesFirst(t *testing.T) {
	out := map[string][]byte{
		"dists/stable/InRelease":                          nil,
		"dists/stable/Release":                            nil,
		"dists/stable/Release.gpg":                        nil,
		"dists/stable/main/binary-amd64/Packages":         nil,
		"dists/stable/main/binary-amd64/by-hash/SHA256/x": nil,
		"pool/main/a/a_1_all.deb":                         nil,
		"_redirects":                                      nil,
	}
	got := writeOrder(out)
	want := []string{
		"_redirects",
		"pool/main/a/a_1_all.deb",
		"dists/stable/main/binary-amd64/by-hash/SHA256/x",
		"dists/stable/main/binary-amd64/Packages",
		"dists/stable/Release.gpg",
		"dists/stable/Release",
		"dists/stable/InRelease",
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("order = %v\nwant    %v", got, want)
	}
}

func TestBuildManifest_Rejects(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(s *refresh.Snapshot)
		want   string
	}{
		{"epoch colon in pool path", func(s *refresh.Snapshot) {
			s.Redirects["/pool/main/f/foo/foo_1:2.3_amd64.deb"] = refresh.Redirect{URL: "https://x.example/foo.deb"}
		}, `contains ':'`},
		{"splat in path", func(s *refresh.Snapshot) {
			s.Files["/dists/*"] = refresh.FileEntry{}
		}, `contains '*'`},
		{"whitespace in path", func(s *refresh.Snapshot) {
			s.Files["/dists/stable/In Release"] = refresh.FileEntry{}
		}, "whitespace"},
		{"traversal", func(s *refresh.Snapshot) {
			s.Files["/dists/../etc/passwd"] = refresh.FileEntry{}
		}, "not canonical"},
		{"marker name reserved", func(s *refresh.Snapshot) {
			s.Files["/"+MarkerName] = refresh.FileEntry{}
		}, "reserved"},
		{"file and redirect clash", func(s *refresh.Snapshot) {
			s.Redirects["/pubkey.gpg"] = refresh.Redirect{URL: "https://x.example/k"}
		}, "both a file and a redirect"},
		{"relative location", func(s *refresh.Snapshot) {
			s.Redirects["/pool/main/z/z/z_1_all.deb"] = refresh.Redirect{URL: "pool/z.deb"}
		}, "neither an absolute http(s) URL nor an absolute path"},
		{"ftp location", func(s *refresh.Snapshot) {
			s.Redirects["/pool/main/z/z/z_1_all.deb"] = refresh.Redirect{URL: "ftp://x.example/z.deb"}
		}, "neither an absolute"},
		{"quote in location", func(s *refresh.Snapshot) {
			s.Redirects["/pool/main/z/z/z_1_all.deb"] = refresh.Redirect{URL: `https://x.example/z".deb`}
		}, "quote"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := testSnapshot()
			tc.mutate(s)
			_, err := BuildManifest(s, "")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}

func TestCloudflare_Limits(t *testing.T) {
	t.Run("too many redirects", func(t *testing.T) {
		s := testSnapshot()
		for i := 0; i <= cloudflareMaxStaticRedirects; i++ {
			s.Redirects["/pool/main/p/p/p_1."+strconv.Itoa(i)+"_amd64.deb"] = refresh.Redirect{URL: "https://x.example/p.deb"}
		}
		m, err := BuildManifest(s, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (cloudflareTarget{}).Emit(m); err == nil || !strings.Contains(err.Error(), "exceeds the _redirects limit") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("line too long", func(t *testing.T) {
		s := testSnapshot()
		s.Redirects["/pool/main/l/long/long_1_amd64.deb"] = refresh.Redirect{URL: "https://x.example/" + strings.Repeat("a", cloudflareMaxRedirectLine)}
		m, err := BuildManifest(s, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (cloudflareTarget{}).Emit(m); err == nil || !strings.Contains(err.Error(), "limited to") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestNginx_Rejects(t *testing.T) {
	t.Run("case-colliding keys", func(t *testing.T) {
		s := testSnapshot()
		s.Redirects["/pool/main/w/widget/widget_1.2.3_AMD64.deb"] = refresh.Redirect{URL: "https://x.example/w.deb"}
		m, err := BuildManifest(s, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (nginxTarget{}).Emit(m); err == nil || !strings.Contains(err.Error(), "differ only by case") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("dollar in location", func(t *testing.T) {
		s := testSnapshot()
		s.Redirects["/pool/main/d/d/d_1_amd64.deb"] = refresh.Redirect{URL: "https://x.example/d.deb?sig=$host"}
		m, err := BuildManifest(s, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := (nginxTarget{}).Emit(m); err == nil || !strings.Contains(err.Error(), "interpolate") {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestParseTargets(t *testing.T) {
	if _, err := ParseTargets([]string{"netlify"}); err == nil || !strings.Contains(err.Error(), "unknown target") {
		t.Errorf("unknown: err = %v", err)
	}
	if _, err := ParseTargets([]string{"nginx", "nginx"}); err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Errorf("dup: err = %v", err)
	}
	if _, err := ParseTargets(nil); err == nil || !strings.Contains(err.Error(), "no target") {
		t.Errorf("none: err = %v", err)
	}
	ts, err := ParseTargets([]string{" cloudflare , manifest "})
	if err != nil || len(ts) != 2 || ts[0].Name() != "cloudflare" || ts[1].Name() != "manifest" {
		t.Errorf("comma list: %v %v", ts, err)
	}
	if got := strings.Join(TargetNames(), ","); got != "cloudflare,manifest,nginx" {
		t.Errorf("TargetNames = %s", got)
	}
}

func TestWrite_PathPrefix(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site")
	snap := testSnapshot()
	if _, err := Write(snap, Options{OutDir: dir, Targets: allTargets(t), PathPrefix: "/apt"}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	// Site tree lands under <out>/apt/; rule files stay at the root.
	if got := readOut(t, dir, "apt/dists/stable/InRelease"); got != "inrelease" {
		t.Errorf("prefixed InRelease = %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "dists")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("unprefixed dists/ should not exist (err=%v)", err)
	}
	for _, root := range []string{"_redirects", "_headers", NginxConfName, ManifestName, MarkerName} {
		if _, err := os.Stat(filepath.Join(dir, root)); err != nil {
			t.Errorf("%s should be at the output root: %v", root, err)
		}
	}

	// Every rule is keyed by the prefixed public path.
	rd := stripComments(readOut(t, dir, "_redirects"))
	if !strings.HasPrefix(rd, "/apt/pool/main/b/bolt/bolt_0.9_arm64.deb https://cdn.example.com/bolt_0.9_arm64.deb 302\n") {
		t.Errorf("_redirects not prefixed:\n%s", rd)
	}
	if strings.Contains(rd, "\n/pool/") || strings.Contains(rd, "\n/release/") {
		t.Errorf("_redirects has unprefixed keys:\n%s", rd)
	}
	hd := readOut(t, dir, "_headers")
	if !strings.Contains(hd, "/apt/dists/*\n") || !strings.Contains(hd, "/apt/pool/*\n") || strings.Contains(hd, "\n/dists/*") {
		t.Errorf("_headers not prefixed:\n%s", hd)
	}
	ng := readOut(t, dir, NginxConfName)
	if !strings.Contains(ng, `    "/apt/pool/main/b/bolt/bolt_0.9_arm64.deb" "https://cdn.example.com/bolt_0.9_arm64.deb";`) ||
		!strings.Contains(ng, "location /apt/dists/") || !strings.Contains(ng, "alias <out>/apt/") {
		t.Errorf("nginx conf not prefixed:\n%s", ng)
	}
	var m Manifest
	if err := json.Unmarshal([]byte(readOut(t, dir, ManifestName)), &m); err != nil {
		t.Fatal(err)
	}
	if m.PathPrefix != "/apt" || m.Files[0].Path != "/apt/dists/stable/InRelease" || m.Redirects[0].Path != "/apt/pool/main/b/bolt/bolt_0.9_arm64.deb" {
		t.Errorf("manifest not prefixed: prefix=%q file0=%s redirect0=%s", m.PathPrefix, m.Files[0].Path, m.Redirects[0].Path)
	}
	// Cache class is decided on the unprefixed path.
	for _, f := range m.Files {
		if strings.Contains(f.Path, "/by-hash/") && f.Cache != CacheImmutable {
			t.Errorf("%s cache = %q", f.Path, f.Cache)
		}
	}
	// Self-referential redirect location already carries the prefix from
	// composeSnapshot and is passed through untouched.
	for _, r := range m.Redirects {
		if r.Path == "/apt/release/stable/latest.deb" && !strings.HasPrefix(r.Location, "/apt/pool/") {
			t.Errorf("latest.deb location = %q", r.Location)
		}
	}
}

func TestWrite_PrefixChangePrunesOldTree(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "site")
	ts, _ := ParseTargets([]string{"manifest"})
	snap := testSnapshot()
	if _, err := Write(snap, Options{OutDir: dir, Targets: ts}); err != nil {
		t.Fatal(err)
	}
	res, err := Write(snap, Options{OutDir: dir, Targets: ts, PathPrefix: "/apt"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Pruned != len(snap.Files) {
		t.Errorf("pruned = %d, want %d (the whole unprefixed tree)", res.Pruned, len(snap.Files))
	}
	for _, old := range []string{"dists", "pool", "pubkey.gpg", "release"} {
		if _, err := os.Stat(filepath.Join(dir, old)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s should be pruned (err=%v)", old, err)
		}
	}
}

func TestBuildManifest_RejectsBadPrefix(t *testing.T) {
	for _, bad := range []string{"apt", "/apt/", "/ap t", "/a:b"} {
		if _, err := BuildManifest(testSnapshot(), bad); err == nil || !strings.Contains(err.Error(), "prefix") {
			t.Errorf("prefix %q: err = %v", bad, err)
		}
	}
	if _, err := BuildManifest(testSnapshot(), "/apt/v2"); err != nil {
		t.Errorf("multi-segment prefix rejected: %v", err)
	}
}
