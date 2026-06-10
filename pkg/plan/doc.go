// Package plan defines the JSON contract that flows between the two phases
// of apt-cooper (discover and build) and through any external producer that
// substitutes for discover.
//
// The contract is versioned. SchemaVersion is bumped on incompatible
// changes to the document shape (field removed, semantic change).
// FormatRevision is bumped when cooper's output .deb bytes would diverge
// for the same logical inputs (e.g. nfpm major upgrade) — independent of
// the schema version and of the cooper release version.
//
// External producers may import this package; everything exported here is
// part of the stable contract.
package plan

const (
	// SchemaVersion is the value cooper writes into Plan.SchemaVersion.
	SchemaVersion = 1

	// FormatRevision is cooper's current build_inputs_hash format
	// revision. Bumped when output .deb bytes would diverge for the same
	// logical inputs. External producers MUST set Tool.FormatRevision to
	// the value of an actual cooper release they are conformant with —
	// build re-checks the hash against that revision's algorithm.
	FormatRevision = 1
)

// Result values for Package.Result.
const (
	ResultOK    = "ok"
	ResultError = "error"
)

// Source.Kind values.
const (
	SourceKindGitHubRelease = "github_release"
	SourceKindGiteaRelease  = "gitea_release"
	SourceKindJSONURL       = "json_url"
	SourceKindXMLURL        = "xml_url"
	SourceKindExternal      = "external"
	SourceKindLocal         = "local"
	SourceKindChandler      = "chandler"
)

// Asset.SHA256Source values. Nil pointer means "unknown; build must
// compute and record."
const (
	SHA256SourceGitHubAPI      = "github_api"
	SHA256SourceHEADRequest    = "head_request"
	SHA256SourceExternalScript = "external_script"
)

// Error.Kind values.
const (
	ErrorKindDiscoveryFailed        = "discovery_failed"
	ErrorKindVersionInvalid         = "version_invalid"
	ErrorKindAuxResolutionFailed    = "aux_resolution_failed"
	ErrorKindBuildFailed            = "build_failed"
	ErrorKindUnresolvedSubstitution = "unresolved_substitution"
	ErrorKindArchGNUUnknown         = "arch_gnu_unknown"
)
