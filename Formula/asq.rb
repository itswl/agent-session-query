# Written by scripts/package_managers.py from the release's SHA256SUMS, for each release.
class Asq < Formula
  desc "Search, read and hand over the session history of every agent CLI"
  homepage "https://github.com/itswl/agent-session-query"
  version "1.6.1"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.1/asq-darwin-arm64.tar.gz"
      sha256 "8217f9027bc53447c17e2e4d1cabfcfe5ad9b3de3b2726ef3c324be7c3c1764f"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.1/asq-darwin-amd64.tar.gz"
      sha256 "13993cfce7c7f87a2de86c57d6353f6a9a1346cbd0c24b98f5bbccc020d44d4b"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.1/asq-linux-arm64.tar.gz"
      sha256 "5783e1208277b29e94c8c592dc629c24c000fb7be295b64b4c73e26b2e1a43e6"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.1/asq-linux-amd64.tar.gz"
      sha256 "ee20db84c1f3f19234fb4146da2f68fd86566fb774402c97a9a80c7616383247"
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
