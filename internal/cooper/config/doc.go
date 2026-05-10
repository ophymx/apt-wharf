// Package config parses and validates apt-cooper's user-facing
// configuration: the top-level cooper.yaml and each per-package multi-doc
// YAML file it points at (cooper sidecar + vanilla nfpm).
//
// The package is network-free. It produces typed values that downstream
// phases (discover, build) consume directly; `cooper validate` runs
// everything here without taking any further action.
package config
