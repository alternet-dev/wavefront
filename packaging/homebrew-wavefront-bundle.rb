# typed: false
# frozen_string_literal: true
#
# Reference shape for the wavefront-bundle CLI formula published to the
# alternet-dev Homebrew tap. The .github/workflows/release.yml `update-tap`
# job generates the real per-tag formula via
# `scripts/release/render-homebrew-formula.sh`, which produces this exact
# structure with the actual version string and per-arch sha256 values
# pulled from the GitHub Release artifacts. This file is the committed
# audit reference; the rendered file at the tap is the binding artifact.
#
# Scope: the CLI binary `wavefront-bundle` only. The proxy server
# (`cmd/wavefront`) is distributed exclusively as the multi-arch container
# image at ghcr.io/alternet-dev/wavefront (see issue #73).

class WavefrontBundle < Formula
  desc "Build and maintain versioned layers in a wavefront edge-proxy bundle"
  homepage "https://github.com/alternet-dev/wavefront"
  version "0.0.0"
  license "MIT OR Apache-2.0"

  on_macos do
    on_arm do
      url "https://github.com/alternet-dev/wavefront/releases/download/v0.0.0/wavefront-bundle-v0.0.0-aarch64-apple-darwin.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
    on_intel do
      url "https://github.com/alternet-dev/wavefront/releases/download/v0.0.0/wavefront-bundle-v0.0.0-x86_64-apple-darwin.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
  end

  on_linux do
    on_intel do
      url "https://github.com/alternet-dev/wavefront/releases/download/v0.0.0/wavefront-bundle-v0.0.0-x86_64-unknown-linux-gnu.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
    on_arm do
      url "https://github.com/alternet-dev/wavefront/releases/download/v0.0.0/wavefront-bundle-v0.0.0-aarch64-unknown-linux-gnu.tar.gz"
      sha256 "0000000000000000000000000000000000000000000000000000000000000000"
    end
  end

  def install
    bin.install "wavefront-bundle"
    doc.install "README.md", "LICENSE-MIT", "LICENSE-APACHE"
  end

  test do
    # wavefront-bundle is a subcommand CLI; a bare invocation prints usage
    # and exits 2.
    output = shell_output("#{bin}/wavefront-bundle 2>&1", 2)
    assert_match(/usage: wavefront-bundle/, output)
  end
end
