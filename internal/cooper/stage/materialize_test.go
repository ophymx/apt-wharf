package stage

import (
	"encoding/base64"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestMaterializeAuxFiles_RawFile(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"hugo.service": "[Unit]\nDescription=Hugo\n",
	})
	doc := parseDoc(t, `contents:
  - { src: ./hugo.service, dst: /lib/systemd/system/hugo.service }`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := MaterializeAuxFiles(refs, PlaceholderVars())
	if err != nil {
		t.Fatal(err)
	}
	aux, ok := got["./hugo.service"]
	if !ok {
		t.Fatalf("no entry for ./hugo.service: %v", got)
	}
	body, _ := base64.StdEncoding.DecodeString(aux.ContentB64)
	if !strings.Contains(string(body), "Description=Hugo") {
		t.Errorf("body decoded wrong: %s", body)
	}
}

func TestMaterializeAuxFiles_RendersTemplate(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"hugo.service.tmpl": "Version={{ .Version }} Arch={{ .Arch }}\n",
	})
	doc := parseDoc(t, `contents:
  - { src: ./hugo.service, dst: /lib/systemd/system/hugo.service }`)
	refs, err := Walk(dir, doc)
	if err != nil {
		t.Fatal(err)
	}
	vars := Vars{Name: "hugo", Version: "0.140.0", Arch: "amd64", PublishedAt: "2026-05-09T08:00:00Z"}
	got, err := MaterializeAuxFiles(refs, vars)
	if err != nil {
		t.Fatal(err)
	}
	aux, ok := got["./hugo.service"]
	if !ok {
		t.Fatalf("missing ./hugo.service entry")
	}
	body, _ := base64.StdEncoding.DecodeString(aux.ContentB64)
	if string(body) != "Version=0.140.0 Arch=amd64\n" {
		t.Errorf("rendered body: %q", body)
	}
}

func TestMaterializeAuxFiles_BadTemplate(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"hugo.service.tmpl": "{{ .Versoin }}",
	})
	doc := parseDoc(t, `contents:
  - { src: ./hugo.service, dst: /x }`)
	refs, _ := Walk(dir, doc)
	_, err := MaterializeAuxFiles(refs, PlaceholderVars())
	if err == nil {
		t.Fatal("expected template execution error")
	}
}

func TestMaterializeAuxFiles_MultipleRefs(t *testing.T) {
	dir := makePkg(t, map[string]string{
		"completions/hugo.bash": "bash-comp",
		"completions/hugo.zsh":  "zsh-comp",
		"hugo-postinstall.sh":   "#!/bin/sh\n",
	})
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(`contents:
  - { src: ./completions/*, dst: /usr/share/bash-completion/completions/ }
scripts:
  postinstall: ./hugo-postinstall.sh`), &doc); err != nil {
		t.Fatal(err)
	}
	refs, err := Walk(dir, &doc)
	if err != nil {
		t.Fatal(err)
	}
	got, err := MaterializeAuxFiles(refs, PlaceholderVars())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"./completions/hugo.bash",
		"./completions/hugo.zsh",
		"./hugo-postinstall.sh",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("missing key %s in %v", want, got)
		}
	}
}
