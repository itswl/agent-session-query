# Written by scripts/package_managers.py from the release's SHA256SUMS, for each release.
class Asq < Formula
  desc "Search, read and hand over the session history of every agent CLI"
  homepage "https://github.com/itswl/agent-session-query"
  version "1.6.3"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.3/asq-darwin-arm64.tar.gz"
      sha256 "41ce2dac0c8fcb1647921fddfb4a0608eac4bb29175f4fef7a38ce7b4e17d4c8"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.3/asq-darwin-amd64.tar.gz"
      sha256 "1a7191d37f5fb85d4707ba2fdc9e4f9946f6075244b22f0d7d4d4f7f2b5cd296"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.3/asq-linux-arm64.tar.gz"
      sha256 "c687c351d5c5a0851a8a49186c63d9bd4bd9913605c1eedd6c2ab86cda0c9799"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.3/asq-linux-amd64.tar.gz"
      sha256 "a555dc11e5ee764ae857bf7470c6294ead4ea24e1e5e4b8a8ada674a3af5d318"
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
