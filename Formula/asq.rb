# Written by scripts/package_managers.py from the release's SHA256SUMS, for each release.
class Asq < Formula
  desc "Search, read and hand over the session history of every agent CLI"
  homepage "https://github.com/itswl/agent-session-query"
  version "1.6.5"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.5/asq-darwin-arm64.tar.gz"
      sha256 "0a78370e2c21d18a0953667b5a910bd014c8c49d804db319f774f5aa02099fe1"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.5/asq-darwin-amd64.tar.gz"
      sha256 "ed9cc6a15ccd0d9ed5691bba2fd25305fe9bbce1e23a0bd1a8156c079c9e4442"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.5/asq-linux-arm64.tar.gz"
      sha256 "3c1909aa9caf843ce8d47af7832dd8c48e5da9fb00ea54fb480fa1f6e804a137"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.6.5/asq-linux-amd64.tar.gz"
      sha256 "8de0c41258247e64a8fc339ab3507f54a043218367b4e05459eb81fe45e30281"
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
