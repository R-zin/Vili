# Homebrew formula for the vili terminal client.
#
# Two distribution paths, both documented in the README:
#   1. A dedicated tap repo (R-zin/homebrew-tap) that GoReleaser keeps updated
#      automatically on each v* tag (see .goreleaser.yml "brews:"). Until that
#      repo exists, this committed Formula/vili.rb is the canonical reference
#      and the SHA256 below is filled in from the release checksums.txt.
#   2. `brew install --build-from-source` against this formula.
#
# NOTE: `sha256` must be updated per release (GoReleaser does this for the tap
# automatically). The `version` follows the git tag without the leading "v".
class Vili < Formula
  desc "Terminal-first chat client for the Vili backend"
  homepage "https://github.com/R-zin/Vili"
  url "https://github.com/R-zin/Vili/archive/refs/tags/v0.1.0.tar.gz"
  sha256 "PLACEHOLDER_UPDATE_FROM_RELEASE_CHECKSUMS_TXT"
  license "MIT"
  head "https://github.com/R-zin/Vili.git", branch: "main"

  depends_on "go" => :build

  def install
    ldflags = %W[
      -s -w
      -X github.com/R-zin/vili/internal/cli.version=#{version}
    ].join(" ")
    system "go", "build", *std_go_args(ldflags:, output: bin/"vili"), "-trimpath", "./cmd/vili"
  end

  test do
    assert_match("vili", shell_output("#{bin}/vili version"))
  end
end
