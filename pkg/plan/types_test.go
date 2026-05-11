package plan

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestPlanRoundTrip parses the design-doc example JSON through Plan and
// re-marshals it; both forms must round-trip the structurally significant
// fields (the example uses ellipses for some bodies, so we round-trip a
// trimmed-but-complete version).
func TestPlanRoundTrip(t *testing.T) {
	in := []byte(`{
  "schema_version": 1,
  "tool": { "name": "cooper", "version": "0.1.0", "format_revision": 1 },
  "discovered_at": "2026-05-10T12:34:56Z",
  "packages": [
    {
      "name": "hugo",
      "result": "ok",
      "source": {
        "kind": "github_release",
        "repo": "gohugoio/hugo",
        "release_id": 178213984,
        "release_tag": "v0.140.0",
        "release_published_at": "2026-05-09T08:12:00Z"
      },
      "artifacts": [
        {
          "arch": "amd64",
          "assets": [{
            "name": "hugo_extended_0.140.0_linux-amd64.tar.gz",
            "url": "https://example.invalid/hugo.tar.gz",
            "size": 19283746,
            "sha256": "sha256:abc123",
            "sha256_source": "github_api"
          }],
          "deb": {
            "filename": "hugo_0.140.0_amd64.deb",
            "build_inputs_hash": "sha256:deadbeef",
            "path": null,
            "sha256": null
          },
          "build_plan": {
            "source_date_epoch": 1715240520,
            "nfpm": {"name": "hugo", "version": "0.140.0", "arch": "amd64"},
            "aux_files": {
              "./hugo.service": { "content_b64": "aGVsbG8=" }
            }
          }
        }
      ]
    },
    {
      "name": "broken",
      "result": "error",
      "error": { "kind": "discovery_failed", "message": "404" }
    }
  ]
}`)

	var p Plan
	if err := json.Unmarshal(in, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if p.SchemaVersion != 1 {
		t.Errorf("schema_version: got %d", p.SchemaVersion)
	}
	if p.Tool.Name != "cooper" || p.Tool.FormatRevision != 1 {
		t.Errorf("tool: %+v", p.Tool)
	}
	if len(p.Packages) != 2 {
		t.Fatalf("packages: got %d", len(p.Packages))
	}

	hugo := p.Packages[0]
	if hugo.Result != ResultOK || hugo.Source == nil || len(hugo.Artifacts) != 1 {
		t.Fatalf("hugo: %+v", hugo)
	}
	if hugo.Source.Kind != SourceKindGitHubRelease {
		t.Errorf("source.kind: %s", hugo.Source.Kind)
	}
	art := hugo.Artifacts[0]
	if art.Arch != "amd64" {
		t.Errorf("arch: %s", art.Arch)
	}
	if len(art.Assets) != 1 {
		t.Fatalf("assets: got %d, want 1", len(art.Assets))
	}
	if art.Assets[0].SHA256 == nil || *art.Assets[0].SHA256 != "sha256:abc123" {
		t.Errorf("assets[0].sha256: %v", art.Assets[0].SHA256)
	}
	if art.Deb.Path != nil || art.Deb.SHA256 != nil {
		t.Errorf("deb path/sha256 should be nil in discover output")
	}
	if art.BuildPlan.SourceDateEpoch != 1715240520 {
		t.Errorf("source_date_epoch: %d", art.BuildPlan.SourceDateEpoch)
	}
	aux, ok := art.BuildPlan.AuxFiles["./hugo.service"]
	if !ok || aux.ContentB64 != "aGVsbG8=" {
		t.Errorf("aux: %+v", art.BuildPlan.AuxFiles)
	}

	broken := p.Packages[1]
	if broken.Result != ResultError {
		t.Errorf("broken.result: %s", broken.Result)
	}
	if broken.Source != nil || broken.Artifacts != nil {
		t.Errorf("broken should have nil source/artifacts")
	}
	if broken.Error == nil || broken.Error.Kind != ErrorKindDiscoveryFailed {
		t.Errorf("broken.error: %+v", broken.Error)
	}

	// Round-trip: marshal back and confirm omitempty for absent unions.
	out, err := json.Marshal(&p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(out, []byte(`"result":"error"`)) {
		t.Errorf("missing error result in re-emit")
	}
	// Source/artifacts should be omitted on the error package.
	if !strings.Contains(string(out), `"name":"broken","result":"error","error":`) {
		t.Errorf("error package re-emitted with unexpected fields: %s", out)
	}
	// On the ok package, error should be omitted; path/sha256 should be explicit nulls.
	if !bytes.Contains(out, []byte(`"path":null,"sha256":null`)) {
		t.Errorf("expected explicit null path/sha256: %s", out)
	}
}

func TestPlan_NfpmRawMessageRoundTrip(t *testing.T) {
	// Whatever JSON shape the user wrote in doc 2 must survive parse → emit.
	in := []byte(`{"build_plan": {"source_date_epoch": 1, "nfpm": {"contents":[{"src":"${ASSETS}/x","dst":"/usr/bin/x"}]}, "aux_files": {}}}`)
	var holder struct {
		BuildPlan BuildPlan `json:"build_plan"`
	}
	if err := json.Unmarshal(in, &holder); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(&holder)
	if err != nil {
		t.Fatal(err)
	}
	// The verbatim string ${ASSETS} must survive — that's the contract.
	if !bytes.Contains(out, []byte(`${ASSETS}/x`)) {
		t.Errorf("nfpm subtree should round-trip verbatim: %s", out)
	}
}
