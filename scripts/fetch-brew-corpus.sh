#!/usr/bin/env bash
#
# Fetch the Tcl/Tk/tcllib manual-page corpus from a Homebrew bottle and build
# the site with it. Homebrew's tcl-tk bottle is the self-contained tree tcldoc
# was developed against: Tcl, Tk and tcllib man pages in one directory, plain
# troff (not gzipped), plus the Tk demos. The man pages are architecture-
# independent text, so the Linux bottle works anywhere -- no macOS involved.
#
# Bottles are OCI artifacts on ghcr.io, so a pull is: anonymous token ->
# index manifest -> per-platform bottle blob (a .tar.gz).
#
# Usage:
#   scripts/fetch-brew-corpus.sh [TAG] [OUT_DIR]
#
#   TAG      bottle version tag, e.g. 9.0.4 (default: latest listed)
#   OUT_DIR  where the generated site goes (default: ./site)
#
# Env:
#   PLATFORM  bottle platform to pull (default: linux/amd64). Any layer's man
#             pages are identical; this only picks which blob to download.
#
# Requires: curl, jq, tar, and a built ./tcldoc (go build -o tcldoc .).

set -euo pipefail

FORMULA=tcl-tk
REPO="homebrew/core/${FORMULA}"
REGISTRY=https://ghcr.io
PLATFORM="${PLATFORM:-linux/amd64}"
OS="${PLATFORM%%/*}"
ARCH="${PLATFORM##*/}"

TAG="${1:-}"
OUT_DIR="${2:-./site}"

for tool in curl jq tar; do
  command -v "$tool" >/dev/null || { echo "error: $tool is required" >&2; exit 1; }
done

echo "==> requesting pull token for ${REPO}"
TOKEN=$(curl -fsS "${REGISTRY}/token?service=ghcr.io&scope=repository:${REPO}:pull" | jq -r .token)

if [[ -z "$TAG" ]]; then
  TAG=$(curl -fsS -H "Authorization: Bearer $TOKEN" \
    "${REGISTRY}/v2/${REPO}/tags/list" | jq -r '.tags[-1]')
  echo "==> no tag given; using latest listed: ${TAG}"
fi

echo "==> resolving ${PLATFORM} bottle for ${FORMULA} ${TAG}"
DIGEST=$(curl -fsS -H "Authorization: Bearer $TOKEN" \
  -H "Accept: application/vnd.oci.image.index.v1+json" \
  "${REGISTRY}/v2/${REPO}/manifests/${TAG}" \
  | jq -r --arg os "$OS" --arg arch "$ARCH" '
      .manifests[]
      | select(.platform.os == $os and .platform.architecture == $arch)
      | .annotations["sh.brew.bottle.digest"]' \
  | head -n1)

if [[ -z "$DIGEST" || "$DIGEST" == "null" ]]; then
  echo "error: no ${PLATFORM} bottle found for tag ${TAG}" >&2
  echo "       list platforms with:" >&2
  echo "       curl -s -H \"Authorization: Bearer \$TOKEN\" -H 'Accept: application/vnd.oci.image.index.v1+json' \\" >&2
  echo "         ${REGISTRY}/v2/${REPO}/manifests/${TAG} | jq '.manifests[].platform'" >&2
  exit 1
fi

TARBALL="${FORMULA}-${TAG}.tar.gz"
echo "==> downloading blob sha256:${DIGEST:0:16}... -> ${TARBALL}"
curl -fsSL -H "Authorization: Bearer $TOKEN" \
  "${REGISTRY}/v2/${REPO}/blobs/sha256:${DIGEST}" -o "$TARBALL"

echo "==> extracting"
tar xzf "$TARBALL"

# The bottle unpacks to tcl-tk/<version>/... . The extracted version dir can
# differ from the requested tag (a tag like 9.0.3-1 unpacks under 9.0.3), so
# discover it rather than assuming it equals $TAG.
MAN=$(find "${FORMULA}" -type d -path "*/share/man" | head -n1)
if [[ -z "$MAN" ]]; then
  echo "error: could not find share/man in the extracted bottle" >&2
  exit 1
fi

PREFIX="${MAN%/share/man}"
DEMOS=$(find "${PREFIX}/lib" -type d -name demos 2>/dev/null | head -n1)

# The Tcl/Tk license.terms sits at the prefix and tcldoc finds it on its own.
# tcllib is bundled here too, but Homebrew ships only its .tcl modules (which say
# "See the file license.terms"), not that file -- so there is nothing on disk to
# discover. Supply it explicitly. It is the same BSD Tcl license, differing only
# in the copyright holder, and is stable boilerplate, so it is embedded here
# rather than fetched. Only added when tcllib is actually present.
license_arg=()
if compgen -G "${PREFIX}/lib/tcllib*" >/dev/null; then
  LIC="${PREFIX}/tcllib-license.terms"
  cat > "$LIC" <<'TCLLIB_LICENSE'
This software is copyrighted by Ajuba Solutions and other parties.
The following terms apply to all files associated with the software
unless explicitly disclaimed in individual files.

The authors hereby grant permission to use, copy, modify, distribute,
and license this software and its documentation for any purpose,
provided that existing copyright notices are retained in all copies
and that this notice is included verbatim in any distributions. No
written agreement, license, or royalty fee is required for any of the
authorized uses.  Modifications to this software may be copyrighted by
their authors and need not follow the licensing terms described here,
provided that the new terms are clearly indicated on the first page of
each file where they apply.

IN NO EVENT SHALL THE AUTHORS OR DISTRIBUTORS BE LIABLE TO ANY PARTY
FOR DIRECT, INDIRECT, SPECIAL, INCIDENTAL, OR CONSEQUENTIAL DAMAGES
ARISING OUT OF THE USE OF THIS SOFTWARE, ITS DOCUMENTATION, OR ANY
DERIVATIVES THEREOF, EVEN IF THE AUTHORS HAVE BEEN ADVISED OF THE
POSSIBILITY OF SUCH DAMAGE.

THE AUTHORS AND DISTRIBUTORS SPECIFICALLY DISCLAIM ANY WARRANTIES,
INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF
MERCHANTABILITY, FITNESS FOR A PARTICULAR PURPOSE, AND
NON-INFRINGEMENT.  THIS SOFTWARE IS PROVIDED ON AN "AS IS" BASIS, AND
THE AUTHORS AND DISTRIBUTORS HAVE NO OBLIGATION TO PROVIDE
MAINTENANCE, SUPPORT, UPDATES, ENHANCEMENTS, OR MODIFICATIONS.

GOVERNMENT USE: If you are acquiring this software on behalf of the
U.S. government, the Government shall have only "Restricted Rights" in
the software and related documentation as defined in the Federal
Acquisition Regulations (FARs) in Clause 52.227.19 (c) (2).  If you
are acquiring the software on behalf of the Department of Defense, the
software shall be classified as "Commercial Computer Software" and the
Government shall have only "Restricted Rights" as defined in Clause
252.227-7013 (c) (1) of DFARs.  Notwithstanding the foregoing, the
authors grant the U.S. Government and others acting in its behalf
permission to use and distribute the software in accordance with the
terms specified in this license.
TCLLIB_LICENSE
  license_arg=(-license "tcllib=${LIC}")
fi

if [[ ! -x ./tcldoc ]]; then
  echo "==> building tcldoc"
  go build -o tcldoc .
fi

echo "==> building site into ${OUT_DIR}"
demos_arg=()
[[ -n "$DEMOS" ]] && demos_arg=(-demos "$DEMOS")
./tcldoc -src "$MAN" "${demos_arg[@]}" "${license_arg[@]}" -version "Tcl/Tk ${TAG}" -out "$OUT_DIR"

echo
echo "done. serve it with:"
echo "  ./tcldoc -src \"$MAN\" ${DEMOS:+-demos \"$DEMOS\" }${license_arg:+-license \"tcllib=$LIC\" }-version \"Tcl/Tk ${TAG}\" -out \"$OUT_DIR\" -serve :8080"
