# Written by scripts/package_managers.py from the release's SHA256SUMS, for each release.
class Asq < Formula
  desc "Search, read and hand over the session history of every agent CLI"
  homepage "https://github.com/itswl/agent-session-query"
  version "1.5.0"
  license "MIT"

  on_macos do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.5.0/asq-darwin-arm64.tar.gz"
      sha256 "39203e9bcb6acf15f8e27ea717d9e32a21654e41484279ef697f173aedee048c"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.5.0/asq-darwin-amd64.tar.gz"
      sha256 "8249d892dc89d1207faac125a152438a9f4a45cae0d9d9963895a5e4f4a7af9c"
    end
  end

  on_linux do
    on_arm do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.5.0/asq-linux-arm64.tar.gz"
      sha256 "8a673d4b59ca704e4c1ebcdb3e0f375a69ff67d35841f480c9200021fa2a3c67"
    end
    on_intel do
      url "https://github.com/itswl/agent-session-query/releases/download/v1.5.0/asq-linux-amd64.tar.gz"
      sha256 "11b0a8003d734c6fba12ac0e63fd75d1ccf69010d974b118549678785010c6cf"
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
