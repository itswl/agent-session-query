#!/usr/bin/env python3
"""The Homebrew formula and the Scoop manifest, written for a release from its SHA256SUMS.

    python3 scripts/package_managers.py v1.6.0 SHA256SUMS   writes Formula/asq.rb and bucket/asq.json
    python3 scripts/package_managers.py --check             says whether they are what this script
                                                            writes for the release they name

`brew tap` and `scoop bucket add` read this repository; what they install is what the two files
on main name. The release workflow writes them for each release and commits them there; CI runs
--check, so that neither is edited by hand into something the next release would undo.
"""
import json
import os
import re
import sys

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
FORMULA = os.path.join(ROOT, 'Formula', 'asq.rb')
MANIFEST = os.path.join(ROOT, 'bucket', 'asq.json')
HOMEPAGE = 'https://github.com/itswl/agent-session-query'
DESC = 'Search, read and hand over the session history of every agent CLI'
# What release.yml builds, by the name each package manager gives the machine
BREW = [('on_macos', 'on_arm', 'asq-darwin-arm64.tar.gz'), ('on_macos', 'on_intel', 'asq-darwin-amd64.tar.gz'),
        ('on_linux', 'on_arm', 'asq-linux-arm64.tar.gz'), ('on_linux', 'on_intel', 'asq-linux-amd64.tar.gz')]
SCOOP = [('64bit', 'asq-windows-amd64.zip'), ('arm64', 'asq-windows-arm64.zip')]


def formula(version, sums):
    base = f'{HOMEPAGE}/releases/download/v{version}'
    systems = ''
    for system in ('on_macos', 'on_linux'):
        systems += f'  {system} do\n'
        for sys_, cpu, archive in BREW:
            if sys_ == system:
                systems += (f'    {cpu} do\n'
                            f'      url "{base}/{archive}"\n'
                            f'      sha256 "{sums[archive]}"\n'
                            f'    end\n')
        systems += '  end\n\n'
    return f'''# Written by scripts/package_managers.py from the release's SHA256SUMS, for each release.
class Asq < Formula
  desc "{DESC}"
  homepage "{HOMEPAGE}"
  version "{version}"
  license "MIT"

{systems}  def install
    bin.install "asq"
  end

  # What docs/deploy.md's LaunchAgent and unit do: started again after a failure only, and
  # Standard, not Background, which throttles it as work nobody is waiting for. It serves
  # on loopback with no settings: asq service install is the way to give it some.
  service do
    run [opt_bin/"asq", "serve"]
    keep_alive successful_exit: false
    process_type :standard
    error_log_path var/"log/asq.log"
  end

  test do
    assert_match "asq v#{{version}}", shell_output("#{{bin}}/asq --version")
  end
end
'''


def manifest(version, sums):
    base = f'{HOMEPAGE}/releases/download/v{version}'
    doc = {
        'version': version,
        'description': DESC,
        'homepage': HOMEPAGE,
        'license': 'MIT',
        'architecture': {arch: {'url': f'{base}/{archive}', 'hash': sums[archive]} for arch, archive in SCOOP},
        'bin': 'asq.exe',
        'notes': 'asq service install keeps asq serve running. While it runs, scoop update asq '
                 'cannot replace asq.exe: schtasks /end /tn asq before, schtasks /run /tn asq after.',
    }
    return json.dumps(doc, indent=4) + '\n'


def read_sums(path):
    sums = {}
    with open(path) as f:
        for line in f:
            fields = line.split()
            if len(fields) == 2 and re.fullmatch(r'[0-9a-f]{64}', fields[0]):
                sums[fields[1].lstrip('*')] = fields[0]
    missing = [a for _, _, a in BREW] + [a for _, a in SCOOP]
    missing = [a for a in missing if a not in sums]
    if missing:
        sys.exit(f'{path} has no checksum for {", ".join(missing)}')
    return sums


def write(path, text):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, 'w', newline='\n') as f:
        f.write(text)


def check():
    """The version and checksums the two files name, and whether each is what this script
    writes for them."""
    with open(FORMULA) as f:
        rb = f.read()
    with open(MANIFEST) as f:
        js = f.read()
    version = re.search(r'^  version "([^"]+)"$', rb, re.M)
    if not version:
        return [f'{FORMULA}: no version']
    version = version.group(1)
    sums = dict((name, s) for name, s in re.findall(r'url "[^"]*/([^"/]+)"\n\s*sha256 "([0-9a-f]{64})"', rb))
    doc = json.loads(js)
    if doc.get('version') != version:
        return [f'the formula is of {version} and the manifest of {doc.get("version")}: both name one release']
    for arch, archive in SCOOP:
        sums[archive] = doc.get('architecture', {}).get(arch, {}).get('hash', '')
    wrong = []
    for path, have, want in ((FORMULA, rb, formula), (MANIFEST, js, manifest)):
        try:
            if have != want(version, sums):
                wrong.append(f'{path} is not what scripts/package_managers.py writes for {version}')
        except KeyError as e:
            wrong.append(f'{path} names no checksum for {e}')
    return wrong


def main(args):
    if args == ['--check']:
        wrong = check()
        for w in wrong:
            print(w, file=sys.stderr)
        return 1 if wrong else 0
    if len(args) != 2 or not re.fullmatch(r'v?\d+\.\d+\.\d+([-+.][0-9A-Za-z.-]+)?', args[0]):
        print(__doc__, file=sys.stderr)
        return 2
    version, sums = args[0].removeprefix('v'), read_sums(args[1])
    write(FORMULA, formula(version, sums))
    write(MANIFEST, manifest(version, sums))
    print(f'wrote {os.path.relpath(FORMULA, ROOT)} and {os.path.relpath(MANIFEST, ROOT)} for v{version}')
    return 0


if __name__ == '__main__':
    sys.exit(main(sys.argv[1:]))
