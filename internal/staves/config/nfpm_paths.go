package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// CollectSrcPaths returns every contents[].src and scripts.{name}
// scalar value in the nfpm doc. Used by staves validate to reject
// upstream-style references (${ASSETS}/${VAR} prefixes, absolute
// paths) that would force a download/extract phase staves doesn't
// have.
//
// Entries with type: symlink are skipped: nfpm interprets symlink
// src: as the link's *target string*, not a file path on disk, so
// the resolver must not try to read it.
func CollectSrcPaths(nfpm *yaml.Node) []string {
	root := nfpm
	if root.Kind == yaml.DocumentNode {
		if len(root.Content) != 1 {
			return nil
		}
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	var srcs []string

	// contents:[]
	for i := 0; i < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value != "contents" {
			continue
		}
		if v.Kind != yaml.SequenceNode {
			break
		}
		for _, entry := range v.Content {
			if entry.Kind != yaml.MappingNode {
				continue
			}
			if isSymlinkEntry(entry) {
				continue
			}
			for j := 0; j < len(entry.Content); j += 2 {
				ek, ev := entry.Content[j], entry.Content[j+1]
				if ek.Value == "src" && ev.Kind == yaml.ScalarNode {
					srcs = append(srcs, ev.Value)
				}
			}
		}
		break
	}

	// scripts.{preinstall,postinstall,preremove,postremove}
	for i := 0; i < len(root.Content); i += 2 {
		k, v := root.Content[i], root.Content[i+1]
		if k.Value != "scripts" {
			continue
		}
		if v.Kind != yaml.MappingNode {
			break
		}
		for j := 0; j < len(v.Content); j += 2 {
			sk, sv := v.Content[j], v.Content[j+1]
			_ = sk
			if sv.Kind == yaml.ScalarNode {
				srcs = append(srcs, sv.Value)
			}
		}
		break
	}
	return srcs
}

// isSymlinkEntry reports whether a contents[] mapping has type: symlink.
// nfpm interprets symlink src: as the link target string, not a file
// path; the resolver must skip these.
func isSymlinkEntry(entry *yaml.Node) bool {
	for j := 0; j < len(entry.Content); j += 2 {
		k, v := entry.Content[j], entry.Content[j+1]
		if k.Value == "type" && v.Kind == yaml.ScalarNode && v.Value == "symlink" {
			return true
		}
	}
	return false
}

// RejectAssetishSrcs returns the first src in srcs that looks like a
// cooper-style passthrough (${VAR}-prefixed or absolute), wrapped in
// an explanatory error. Staves recipes must reference only local
// files; an absolute or ${...} src would silently fail at nfpm exec
// without this guard.
func RejectAssetishSrcs(srcs []string) error {
	for _, s := range srcs {
		switch {
		case strings.HasPrefix(s, "${"):
			return fmt.Errorf("contents/scripts src %q uses ${VAR} substitution; staves recipes must reference local files only (no upstream asset)", s)
		case strings.HasPrefix(s, "/"):
			return fmt.Errorf("contents/scripts src %q is an absolute path; staves recipes must reference local files inside the package directory", s)
		}
	}
	return nil
}
