#!/usr/bin/env bash
# Render the Homebrew formula for wavefront-bundle at a given tag.
#
# Usage: render-homebrew-formula.sh <ref-name> <repo>
#
# Pulls the .sha256 files from the GitHub Release (created earlier in the
# release workflow) and emits a formula to stdout. Run inside the CI
# workflow after the binaries have been uploaded.
#
# For offline tests: setting WAVEFRONT_SHA_OVERRIDE_<TARGET> (target
# uppercased, dashes -> underscores) short-circuits the curl. Production
# never sets these.

set -euo pipefail

REF_NAME="${1:?ref name required (e.g., v0.5.0)}"
REPO="${2:?repo required (e.g., alternet-dev/wavefront)}"
VERSION="${REF_NAME#v}"
BASE_URL="https://github.com/${REPO}/releases/download/${REF_NAME}"

fetch_sha() {
  local target="$1"
  # Portable uppercase (avoids the Bash-4 `${var^^}` expansion so the
  # script runs under macOS's Bash 3.2 too).
  local target_upper
  target_upper="$(printf '%s' "$target" | tr '[:lower:]-' '[:upper:]_')"
  local override_var="WAVEFRONT_SHA_OVERRIDE_${target_upper}"
  if [[ -n "${!override_var:-}" ]]; then
    printf '%s\n' "${!override_var}"
    return 0
  fi
  local url="${BASE_URL}/wavefront-bundle-${REF_NAME}-${target}.tar.gz.sha256"
  # The .sha256 file is `<sha>  <filename>`; emit just the sha.
  curl --fail --silent --show-error --location "$url" | awk '{print $1}'
}

# Intel-Mac (x86_64-apple-darwin) is intentionally not built — see the
# matrix comment in .github/workflows/release.yml. Intel-Mac users on
# Homebrew will get a "no available formula" message; they can use the
# x86_64-unknown-linux-gnu binary via Rosetta or build from source.
SHA_DARWIN_ARM=$(fetch_sha aarch64-apple-darwin)
SHA_LINUX_X86=$(fetch_sha x86_64-unknown-linux-gnu)
SHA_LINUX_ARM=$(fetch_sha aarch64-unknown-linux-gnu)

cat <<EOF
class WavefrontBundle < Formula
  desc "Build and maintain versioned layers in a wavefront edge-proxy bundle"
  homepage "https://github.com/${REPO}"
  version "${VERSION}"
  license "MIT OR Apache-2.0"

  on_macos do
    on_arm do
      url "${BASE_URL}/wavefront-bundle-${REF_NAME}-aarch64-apple-darwin.tar.gz"
      sha256 "${SHA_DARWIN_ARM}"
    end
  end

  on_linux do
    on_intel do
      url "${BASE_URL}/wavefront-bundle-${REF_NAME}-x86_64-unknown-linux-gnu.tar.gz"
      sha256 "${SHA_LINUX_X86}"
    end
    on_arm do
      url "${BASE_URL}/wavefront-bundle-${REF_NAME}-aarch64-unknown-linux-gnu.tar.gz"
      sha256 "${SHA_LINUX_ARM}"
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
EOF
