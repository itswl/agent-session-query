# Written by scripts/package_managers.py from the release's SHA256SUMS, for each release.
class Asq < Formula
  desc "Search, read and hand over the session history of every agent CLI"
  homepage "https://github.com/itswl/agent-session-query"
  version "1.6.4"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.4/asq-darwin-arm64.tar.gz"
      sha256 "7b2d609ffb59874e25d77d875f7589bebb2f93cfc819d7e1f4199bde901cf656"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.4/asq-darwin-amd64.tar.gz"
      sha256 "7ce082f0b9e9477a30069c976077ba3bcc539ce467f707c9bb1fb6a42d9137b8"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.4/asq-linux-arm64.tar.gz"
      sha256 "94866b898c7e4f7103a76903b56f07fb0b98c8636f37568cf6323300df965608"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.4/asq-linux-amd64.tar.gz"
      sha256 "c992843f6ce4c0c7e2e81ad8b62d8fc91178af5de7e549dee792b1642079b12c"
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
