# Written by scripts/package_managers.py from the release's SHA256SUMS, for each release.
class Asq < Formula
  desc "Search, read and hand over the session history of every agent CLI"
  homepage "https://github.com/itswl/agent-session-query"
  version "1.6.2"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.2/asq-darwin-arm64.tar.gz"
      sha256 "8001b8cb3f53363f2e44a1ea591aaa973d25a05fb614c0bde899d2d950774aab"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.2/asq-darwin-amd64.tar.gz"
      sha256 "ccf762acdc0fd2253052469f58833cbe72d3f2c1ba541c9f19b0b9b11ad22366"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.2/asq-linux-arm64.tar.gz"
      sha256 "80311a6761a3c1b82e96fed7111d6afc214a1a614c112bea9400af95d3141a73"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.2/asq-linux-amd64.tar.gz"
      sha256 "d9fa21ad693174b56688b75f1a0a488bec8391ba29b14fb6af8962f2bb83fdb8"
    end
  end

  def install
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
    assert_match "asq v#{version}", shell_output("#{bin}/asq --version")
  end
end
