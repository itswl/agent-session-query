package app

import "runtime/debug"

// A binary a release did not build — go install, a container built from a checkout, go
// build — is given no version at link time. Go records the module's in it all the same,
// and that is the version it reports.
func init() {
	if info, ok := debug.ReadBuildInfo(); ok {
		Version = moduleVersion(Version, info.Main.Version)
	}
}

// moduleVersion is the version a binary reports: the one set at link time, or, when none
// was, the module's as Go recorded it — the release `go install …@latest` fetched, or for
// a build in a checkout a pseudo-version naming its commit
// (v1.5.1-0.20261122134833-b66c73f1d4cf, +dirty when the checkout had changes). A build
// that recorded neither stays "dev".
func moduleVersion(set, module string) string {
	if set != "dev" || module == "" || module == "(devel)" {
		return set
	}
	return module
}
