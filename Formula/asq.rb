# Written by scripts/package_managers.py from the release's SHA256SUMS, for each release.
class Asq < Formula
  desc "Search, read and hand over the session history of every agent CLI"
  homepage "https://github.com/itswl/agent-session-query"
  version "1.6.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.0/asq-darwin-arm64.tar.gz"
      sha256 "8b47eb218d79a49835a8771cd283d4e282186409fa553f98f2b61c9c26f38d29"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.0/asq-darwin-amd64.tar.gz"
      sha256 "921d08ef760f73c6005f150e64dbaae42bbdad5fdcc4998f1eee0d9a50a4edc9"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.0/asq-linux-arm64.tar.gz"
      sha256 "ea7f85276047c6dacc76cdad75ba3a90c4949f730c95ecaf5c584cf968e0262c"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.0/asq-linux-amd64.tar.gz"
      sha256 "c1c7566680ea3e69eb0a8c7e553eb3d6d600d559e66de58ee4f5f5d6cb398f7f"
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
