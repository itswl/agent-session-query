package app

import "testing"

// A binary given no version at link time reports the module's as Go recorded it, and one
// given a version keeps it.
func TestAVersionFromTheModule(t *testing.T) {
	for _, tc := range []struct{ set, module, want string }{
		{"dev", "v1.5.0", "v1.5.0"}, // go install …@latest
		{"dev", "v1.5.1-0.20261122134833-b66c73f1d4cf+dirty", "v1.5.1-0.20261122134833-b66c73f1d4cf+dirty"}, // go build in a checkout
		{"dev", "(devel)", "dev"}, // a build that recorded no version: a container built from a copy of the source
		{"dev", "", "dev"},
		{"v1.5.0", "v1.5.1-0.20261122134833-b66c73f1d4cf", "v1.5.0"}, // a release
		{"ci", "(devel)", "ci"},
	} {
		if got := moduleVersion(tc.set, tc.module); got != tc.want {
			t.Errorf("moduleVersion(%q, %q) = %q; want %q", tc.set, tc.module, got, tc.want)
		}
	}
}
