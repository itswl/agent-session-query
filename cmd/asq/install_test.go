package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeCurl stands in for curl: the latest release is v9.9.9, and a download is the file of
// that name in $RELEASE.
const fakeCurl = `#!/bin/sh
out=""; url=""; head=""
while [ $# -gt 0 ]; do
  case "$1" in
    -o) out="$2"; shift ;;
    -w) shift ;;
    -fsSLI) head=1 ;;
    -*) ;;
    *) url="$1" ;;
  esac
  shift
done
if [ -n "$head" ]; then printf '%s' "https://github.com/example/asq/releases/tag/v9.9.9"; exit 0; fi
cp "$RELEASE/$(basename "$url")" "$out"
`

// program writes an executable that says it is asq of a version, or says what it is.
func program(t *testing.T, path, says string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho '"+says+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

// release writes a release of v9.9.9 for this machine, and its checksums.
func release(t *testing.T, dir string) {
	t.Helper()
	archive := fmt.Sprintf("asq-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	body := []byte("#!/bin/sh\necho 'asq v9.9.9'\n")
	f, err := os.Create(filepath.Join(dir, archive))
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "asq", Mode: 0o755, Size: int64(len(body))})
	tw.Write(body)
	tw.Close()
	gz.Close()
	f.Close()
	raw, err := os.ReadFile(filepath.Join(dir, archive))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(hex.EncodeToString(sum[:])+"  "+archive+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Run again, the installer upgrades the asq on the PATH where it is — and not a program of
// that name that is not asq, nor over a directory that was named.
func TestTheInstallerUpgradesWhereAsqIs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux; Windows has install.ps1")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("no release for this machine")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		about    string
		existing string // what the asq on the PATH says, "" for none
		named    bool   // INSTALL_DIR names a directory
		where    string // where v9.9.9 is to be, under the home directory
		said     []string
	}{
		{"a first install", "", false, ".local/bin/asq", nil},
		{"an upgrade", "asq v1.4.0", false, "tools/asq", []string{"replacing asq v1.4.0 at ", "goes on as v1.4.0 until it is restarted"}},
		{"another program of that name", "asq: the amazing sequencer, 2.0", false, ".local/bin/asq", []string{"another asq is on your PATH"}},
		{"a directory named", "asq v1.4.0", true, "named/asq", []string{"another asq is on your PATH"}},
	} {
		home := t.TempDir()
		fake, rel := filepath.Join(home, "fake"), filepath.Join(home, "release")
		os.MkdirAll(rel, 0o755)
		os.MkdirAll(fake, 0o755)
		if err := os.WriteFile(filepath.Join(fake, "curl"), []byte(fakeCurl), 0o755); err != nil {
			t.Fatal(err)
		}
		release(t, rel)
		tools := filepath.Join(home, "tools")
		if tc.existing != "" {
			program(t, filepath.Join(tools, "asq"), tc.existing)
		}
		cmd := exec.Command("sh", script)
		cmd.Env = []string{"PATH=" + fake + ":" + tools + ":" + filepath.Join(home, ".local", "bin") + ":/usr/bin:/bin", "HOME=" + home, "RELEASE=" + rel, "SHELL=/bin/sh", "NO_SUDO=1"}
		if tc.named {
			cmd.Env = append(cmd.Env, "INSTALL_DIR="+filepath.Join(home, "named"))
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v\n%s", tc.about, err, out)
		}
		got, err := exec.Command(filepath.Join(home, tc.where), "--version").Output()
		if err != nil || strings.TrimSpace(string(got)) != "asq v9.9.9" {
			t.Errorf("%s: %s says %q, %v\n%s", tc.about, tc.where, got, err, out)
		}
		for _, s := range tc.said {
			if !strings.Contains(string(out), s) {
				t.Errorf("%s: the installer did not say %q:\n%s", tc.about, s, out)
			}
		}
		if tc.existing != "" && tc.where != "tools/asq" {
			if left, _ := exec.Command(filepath.Join(tools, "asq")).Output(); strings.TrimSpace(string(left)) != tc.existing {
				t.Errorf("%s: the program already there was changed: it says %q", tc.about, left)
			}
		}
	}
}

// An asq Homebrew installed is left to Homebrew: written over, its link into the Cellar would
// be lost to Homebrew, and a service run by Homebrew's opt path would go on with the old one.
func TestTheInstallerLeavesHomebrewsAsqToHomebrew(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("install.sh is for macOS and Linux; Windows has install.ps1")
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		t.Skip("no release for this machine")
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, named := range []bool{false, true} {
		home := t.TempDir()
		fake, rel := filepath.Join(home, "fake"), filepath.Join(home, "release")
		os.MkdirAll(rel, 0o755)
		os.MkdirAll(fake, 0o755)
		if err := os.WriteFile(filepath.Join(fake, "curl"), []byte(fakeCurl), 0o755); err != nil {
			t.Fatal(err)
		}
		release(t, rel)
		brew := filepath.Join(home, "homebrew")
		program(t, filepath.Join(brew, "Cellar", "asq", "1.4.0", "bin", "asq"), "asq v1.4.0")
		os.MkdirAll(filepath.Join(brew, "bin"), 0o755)
		link := filepath.Join(brew, "bin", "asq")
		if err := os.Symlink(filepath.Join("..", "Cellar", "asq", "1.4.0", "bin", "asq"), link); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", script)
		cmd.Env = []string{"PATH=" + fake + ":" + filepath.Join(brew, "bin") + ":/usr/bin:/bin", "HOME=" + home, "RELEASE=" + rel, "SHELL=/bin/sh", "NO_SUDO=1"}
		if named {
			cmd.Env = append(cmd.Env, "INSTALL_DIR="+filepath.Join(home, "named"))
		}
		out, err := cmd.CombinedOutput()
		if target, lerr := os.Readlink(link); lerr != nil || !strings.Contains(target, "Cellar") {
			t.Errorf("named %v: Homebrew's link is now %q, %v\n%s", named, target, lerr, out)
		}
		if !named {
			if err == nil || !strings.Contains(string(out), "brew upgrade asq") {
				t.Errorf("the installer went on over Homebrew's asq: %v\n%s", err, out)
			}
			continue
		}
		// A directory named is installed in, beside Homebrew's
		if err != nil {
			t.Fatalf("named: %v\n%s", err, out)
		}
		if got, err := exec.Command(filepath.Join(home, "named", "asq"), "--version").Output(); err != nil || strings.TrimSpace(string(got)) != "asq v9.9.9" {
			t.Errorf("named: says %q, %v\n%s", got, err, out)
		}
	}
}
