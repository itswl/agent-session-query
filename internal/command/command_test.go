package command

import "testing"

// What counts as running a command again.
func TestCommandKey(t *testing.T) {
	for in, want := range map[string]string{
		"go test ./...":                        "go test",
		"go  test   -run TestX ./internal/x":   "go test",
		"cd /work/api && go test ./... | tail": "go test",
		"cd /work/api; make lint":              "make lint",
		"FOO=1 BAR=2 pnpm build --prod":        "pnpm build",
		"make":                                 "make",
		"./run.sh --fast":                      "./run.sh",
		"git push origin main":                 "git push",
		"python3 -c 'print(1)'":                "", // inline code: the next is other code
		"$(which go) test":                     "",
		`cd "/x/My Project" && npm test`:       "npm test",
		`cd ~/My\ Project && npm test`:         "npm test",
		"cd /x\ngo test ./...":                 "go test",
		`bash -lc "go test ./..."`:             "go test",
		"zsh -lc 'cargo build'":                "cargo build",
		"git -C /work/api push origin main":    "git push",
		"git -C /work/api status":              "git status",
		"git --no-pager log -5":                "git log",
		"sudo -n systemctl restart nginx":      "systemctl restart",
		"sudo -u deploy make release":          "make release",
		"npm run build":                        "npm run build",
		"npm run lint":                         "npm run lint",
		"uv run pytest -x":                     "uv run pytest",
		"uv run python -c 'print(2)'":          "",
		"npx -y tsc --noEmit":                  "npx tsc",
		"source .venv/bin/activate && pytest":  "pytest",
		"timeout 60 go test ./...":             "go test",
		"env FOO=1 go test ./...":              "go test",
		"time go test ./...":                   "go test",
		"go test ./... 2>&1 | tail -5":         "go test",
		"python3 -m pytest tests/":             "python3 -m pytest",
		"python3 script.py -c config":          "python3 script.py",
		"/usr/bin/make check":                  "make check",
		`"C:\tools\make.exe" check`:            "make check", // quoted, as a Windows path must be to a shell
		// Codex keeps its argv commands joined by spaces, the script unquoted
		"bash -lc cd /p && go test ./...":   "go test",
		"bash -lc go build ./...":           "go build",
		"/bin/zsh -lc cd /x && make lint":   "make lint",
		"bash -lc 'cd /p && go test ./...'": "go test",
		"(cd web && npm test)":              "npm test",
		"! go test ./...":                   "",
		"if go test ./...; then echo; fi":   "",
		"python3 - <<'EOF'\nprint(1)\nEOF":  "",
		"python3 <<EOF\nprint(1)\nEOF":      "",
		"cat > t.py <<'EOF'\nx\nEOF":        "",
		"pnpm --filter web build":           "pnpm build",
		"pnpm -C web build":                 "pnpm build",
		"npm --prefix web run build":        "npm run build",
		"make -C web build":                 "make build",
		"git --work-tree /x status":         "git status",
		"uv run --with x pytest":            "uv run pytest",
		"# run the tests\ngo test ./...":    "go test",
		// Codex argv scripts that run over lines, or with what runs the shell in front
		"bash -lc cd /p\ngo test ./...":                          "go test",
		"bash -lc python3 - <<'PY'\nimport sys\nsys.exit(1)\nPY": "",
		"bash -lc set -euo pipefail\ncd /p\nmake test":           "make test",
		"timeout 60 bash -lc cd /p && go test ./...":             "go test",
		// A shell's long options, and options with arguments, before its -c
		"bash --login -c 'go test ./...'":            "go test",
		"bash --noprofile --norc -c 'make lint'":     "make lint",
		"bash -o pipefail -c 'go test ./... | tail'": "go test",
		"fish -c 'go test ./...'":                    "go test",
		"bash ./run.sh -c":                           "bash ./run.sh",
		// Flags that take no value
		"pnpm -w lint":                        "pnpm lint",
		"make -e test":                        "make test",
		"make -j test":                        "make test",
		"make -j 8 test":                      "make test",
		"make -C build -j 8 test":             "make test",
		"make -k -j 4 test":                   "make test",
		"make --jobs 4 test":                  "make test",
		"gmake -f GNUmakefile -j 8 all":       "gmake all",
		"make -k -l 2 check":                  "make check",
		"make -j $(nproc) test":               "make test",
		`make -j "$JOBS" lint`:                "make lint",
		"bash -euo pipefail -c 'make test'":   "make test",
		"zsh -eo pipefail -c 'go test ./...'": "go test",
		"bash -lc 'make' extra":               "make",
		"time (cd web && npm test)":           "npm test",
		"":                                    "",
		"   ":                                 "",
	} {
		if got := Key(in); got != want {
			t.Errorf("Key(%q) = %q, want %q", in, got, want)
		}
	}
}

// Whether a line's exit status is its command's: what follows the command on the line
// can hide that it failed.
func TestTheLinesStatusIsTheCommands(t *testing.T) {
	for in, want := range map[string]bool{
		"go test ./...":                             true,
		"go test ./... 2>&1":                        true,
		"go test ./... &> log":                      true,
		"cd x && make && echo ok":                   true,
		"make && echo ok | tee log":                 true,
		"set -o pipefail; go test ./... | tail -5":  true,
		"go test ./... | tail -5":                   false,
		"go test ./...; echo done":                  false,
		"go test ./...\necho done":                  false,
		"make || true":                              false,
		"make && echo a || echo b":                  false,
		"make &":                                    false,
		`echo "a; b" && make`:                       true, // a quoted ";" is no operator
		`go test -run 'A|B' ./...`:                  true,
		"go test $(go list ./... | grep -v vendor)": true,
		// Through the shells and wrappers a line is read through
		"bash -lc 'go test ./...'":                        true,
		"timeout 300 bash -c 'go test ./... 2>&1 | tail'": false,
		"bash -lc go test ./... | tail -5":                false,
		"bash -lc 'go test ./...' | tail -5":              false,
		"(cd web && npm test)":                            true,
		"(cd web && npm test) || true":                    false,
		"(cd web && npm test) &":                          false,
		// What only ends the line hides nothing
		"go test ./...\n":                        true,
		"go test ./... ;":                        true,
		"go test ./... &&\necho ok":              true,
		"go test ./... # note; more":             true,
		"# run the tests\ngo test ./...":         true,
		"bash --login -c 'go test ./... | tail'": false,
		"bash -lc cd /p\ngo test ./...":          true,
	} {
		if _, got := resolve(in, 0); got != want {
			t.Errorf("resolve(%q) decides = %v, want %v", in, got, want)
		}
	}
}

// No line, however it is made, stops the reading of a command: it is a transcript's word.
func FuzzCommandKey(f *testing.F) {
	for _, seed := range []string{"go test ./...", "bash -lc cd /p && go test ./... | tail", "(cd x && make) || true", `cd "/a b" && npm run build`,
		"python3 - <<'EOF'\nx\nEOF", "echo $(date; ls) `x|y` 2>&1 &", "sudo -u a timeout 5 env A=1 nice -n 2 bash -c 'make'", "((", "'", `"\`, "\\"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, line string) {
		Key(line)
		resolve(line, 0)
	})
}
