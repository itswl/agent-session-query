package index

import "testing"

// A directory is compared with a working directory as its own path and as how the paths
// below it begin, in the separator it is written with: a session recorded on Windows is
// below C:\work\api by a backslash.
func TestADirectoryIsItsPathAndWhatBeginsBelowIt(t *testing.T) {
	for _, tc := range []struct{ dir, same, under string }{
		{"/work/api", "/work/api", "/work/api/"},
		{"/work/api/", "/work/api", "/work/api/"},
		{"/", "/", "/"},
		{`C:\work\api`, `C:\work\api`, `C:\work\api\`},
		{`C:\work\api\`, `C:\work\api`, `C:\work\api\`},
		{`C:\`, `C:\`, `C:\`},
		{"C:/work/api", "C:/work/api", "C:/work/api/"},
	} {
		same, under := dirTerms(tc.dir)
		if same != tc.same || under != tc.under {
			t.Errorf("dirTerms(%q) = %q, %q; want %q, %q", tc.dir, same, under, tc.same, tc.under)
		}
	}
}
