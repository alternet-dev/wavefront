# typed: false
# frozen_string_literal: true

class Wavefront < Formula
  desc "Build and maintain versioned layers in a wavefront edge-proxy bundle"
  homepage "https://github.com/alternet-dev/wavefront"
  url "https://github.com/alternet-dev/wavefront.git",
      tag:      "TAG_PLACEHOLDER",
      revision: "REVISION_PLACEHOLDER"
  license any_of: ["Apache-2.0", "MIT"]

  livecheck do
    url :stable
    strategy :git
  end

  depends_on "go" => :build

  def install
    system "go", "build", *std_go_args(ldflags: "-s -w", output: bin/"wavefront-bundle"),
           "./cmd/wavefront-bundle"
  end

  test do
    # wavefront-bundle is a subcommand CLI; a bare invocation prints usage
    # and exits 2.
    output = shell_output("#{bin}/wavefront-bundle 2>&1", 2)
    assert_match(/usage: wavefront-bundle/, output)
  end
end
