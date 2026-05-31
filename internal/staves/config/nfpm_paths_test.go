package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func parseDoc(t *testing.T, body string) *yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(body), &n); err != nil {
		t.Fatal(err)
	}
	return &n
}

func TestCollectSrcPaths_SkipsSymlinkEntries(t *testing.T) {
	// Real packaging cases (kubelogin's kubectl-oidc-login symlink,
	// zigbee2mqtt's /opt/<pkg>/index.js → node_modules/...) used to
	// trip CollectSrcPaths because the symlink's src is a *target
	// string* — looks like an absolute path. RejectAssetishSrcs would
	// then flag it as "absolute path; staves recipes must reference
	// local files only." Skipping type: symlink entries fixes it.
	doc := parseDoc(t, `contents:
  - { src: ./real-file, dst: /etc/foo }
  - { src: /usr/lib/node_modules/foo/server.js, dst: /opt/foo/index.js, type: symlink }
  - { src: ./postinst.sh, dst: /not-used }
scripts:
  preinstall: ./preinst.sh
`)
	got := CollectSrcPaths(doc)
	want := []string{"./real-file", "./postinst.sh", "./preinst.sh"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("got[%d]=%q, want %q", i, got[i], want[i])
		}
	}
}

func TestCollectSrcPaths_SymlinkAllowsAbsoluteTarget(t *testing.T) {
	// Round-trip the validate flow: symlink with absolute "src" must
	// pass RejectAssetishSrcs (because the collector skips it).
	doc := parseDoc(t, `contents:
  - { src: /usr/bin/kubelogin, dst: /usr/local/bin/kubectl-oidc-login, type: symlink }
`)
	srcs := CollectSrcPaths(doc)
	if err := RejectAssetishSrcs(srcs); err != nil {
		t.Errorf("symlink target should not be rejected as an absolute src: %v", err)
	}
}

func TestCollectSrcPaths_RegularAbsoluteStillRejected(t *testing.T) {
	// Sanity check: a non-symlink entry with absolute src: must still
	// trip RejectAssetishSrcs. The skip is type:-gated, not
	// path-content-gated.
	doc := parseDoc(t, `contents:
  - { src: /etc/passwd, dst: /etc/passwd }
`)
	err := RejectAssetishSrcs(CollectSrcPaths(doc))
	if err == nil || !strings.Contains(err.Error(), "absolute path") {
		t.Errorf("expected absolute-path rejection for non-symlink entry, got %v", err)
	}
}
