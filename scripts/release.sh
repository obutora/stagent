#!/usr/bin/env bash
# Build the six stagent release binaries, write SHA256SUMS and, inside the
# SSH Term app tree, regenerate the app's pinned release constants
# (lib/services/agent_bridge/stagent_release.dart: version, download URL,
# sha256 and size of every asset).
#
#   scripts/release.sh                      build into dist/ (and regenerate the Dart file)
#   scripts/release.sh --publish            additionally publish v<version> to
#                                           github.com/obutora/stagent (needs gh)
#   scripts/release.sh --publish --dry-run  every publish step except pushing
#                                           and creating the release
#
# --publish makes the public repository's tag the source that was built. It
# clones obutora/stagent, replaces its tree with this directory's git-tracked
# files as they are on disk (dist/ excluded; untracked files are refused),
# commits them as "stagent v<version> source" if that changes anything,
# rebuilds the binaries from that commit and aborts unless they equal dist/
# byte for byte, pushes main and the tag v<version> together (refusing a tag
# that already exists on another commit), then creates the release at that
# commit with dist/ as its assets and, as its notes, the version's section of
# CHANGELOG.md (required) followed by the Go version.
#
# Builds are reproducible (-trimpath, no VCS stamp, empty build id), so the
# same source and Go toolchain always give the same binaries. Run it from a
# checkout of the public source repository to check a release (README.md,
# "Building and verifying a release"). Set GO to use a specific toolchain.
#
# The macOS assets carry a Developer ID signature (identifier
# com.obutora.stagent) instead of the Go linker's ad-hoc one: macOS keeps a
# privacy permission (Full Disk Access, removable volumes) given to an
# ad-hoc signed binary only for those exact bytes, so every update lost it
# and sessions on such a volume hung on the permission prompt. With
# STAGENT_SIGN_KEY (the PEM private key) and STAGENT_SIGN_CERT (its
# "Developer ID Application" certificate, DER or PEM) set, the darwin
# binaries are signed on macOS through a temporary keychain (so it works
# over SSH, where the login keychain is locked) with a secure timestamp;
# SHA256SUMS and the app's pins are of the signed files. --publish requires
# them. The rebuild check compares the darwin binaries with their
# signatures replaced by the same ad-hoc one.
set -euo pipefail

publish=0
dry_run=0
for arg in "$@"; do
  case "$arg" in
    --publish) publish=1 ;;
    --dry-run) dry_run=1 ;;
    -h|--help) awk 'NR > 1 { if (!/^#/) exit; sub(/^# ?/, ""); print }' "$0"; exit 0 ;;
    *) echo "release.sh: unknown argument: $arg" >&2; exit 2 ;;
  esac
done
if [ "$dry_run" = 1 ] && [ "$publish" = 0 ]; then
  echo "release.sh: --dry-run only applies to --publish" >&2
  exit 2
fi

die() { echo "release.sh: $*" >&2; exit 1; }

here="$(cd "$(dirname "$0")/.." && pwd)"     # stagent module root
app="$(cd "$here/.." && pwd)"                 # Flutter app root
dist="$here/dist"
dart_out="$app/lib/services/agent_bridge/stagent_release.dart"
repo="obutora/stagent"
go="${GO:-go}"

version="$(sed -n 's/^const Version = "\(.*\)"$/\1/p' "$here/internal/version/version.go")"
[ -n "$version" ] || die "cannot read Version from internal/version/version.go"
tag="v$version"
base_url="https://github.com/$repo/releases/download/$tag"
targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"
go_version="$("$go" env GOVERSION)"
sign_id="com.obutora.stagent"
sign_key="${STAGENT_SIGN_KEY:-}"
sign_cert="${STAGENT_SIGN_CERT:-}"

if [ -n "$sign_key" ] || [ -n "$sign_cert" ]; then
  [ -n "$sign_key" ] && [ -n "$sign_cert" ] || die "set both STAGENT_SIGN_KEY and STAGENT_SIGN_CERT, or neither"
  [ -f "$sign_key" ] || die "STAGENT_SIGN_KEY: no file $sign_key"
  [ -f "$sign_cert" ] || die "STAGENT_SIGN_CERT: no file $sign_cert"
  [ "$(uname -s)" = Darwin ] || die "signing the macOS binaries needs macOS (codesign)"
elif [ "$publish" = 1 ]; then
  die "--publish signs the macOS binaries: set STAGENT_SIGN_KEY and STAGENT_SIGN_CERT (see the top of this script)"
fi

work="$(mktemp -d)"
keychain=""
old_keychains=()
cleanup() {
  if [ -n "$keychain" ]; then
    [ "${#old_keychains[@]}" = 0 ] || security list-keychains -d user -s "${old_keychains[@]}"
    security delete-keychain "$keychain" 2>/dev/null || true
  fi
  rm -rf "$work"
}
trap cleanup EXIT

if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
else
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
fi
size() { wc -c < "$1" | tr -d ' '; }

asset() { # asset <os>-<arch> → release file name
  case "$1" in windows-*) echo "stagent-$1.exe" ;; *) echo "stagent-$1" ;; esac
}

# build <module dir> <output dir>: the six binaries.
build() {
  local src="$1" out="$2" t os arch name
  for t in $targets; do
    os="${t%/*}" arch="${t#*/}"
    name="$(asset "$os-$arch")"
    echo "  build $name"
    (cd "$src" && CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" "$go" build \
      -trimpath -buildvcs=false -ldflags '-s -w -buildid=' \
      -o "$out/$name" ./cmd/stagent)
  done
}

# sums <dir>: SHA256SUMS of the six binaries in dir.
sums() {
  local t name
  : > "$1/SHA256SUMS"
  for t in $targets; do
    name="$(asset "${t%/*}-${t#*/}")"
    printf '%s  %s\n' "$(sha "$1/$name")" "$name" >> "$1/SHA256SUMS"
  done
}

# cert_pem <certificate> <out>: the certificate as PEM (it may be DER, as
# developer.apple.com hands it out).
cert_pem() {
  openssl x509 -in "$1" -out "$2" 2>/dev/null || openssl x509 -inform DER -in "$1" -out "$2"
}

# sign_darwin <dir>: signs the darwin binaries in dir with the Developer ID
# identity of STAGENT_SIGN_KEY / STAGENT_SIGN_CERT and checks the result
# carries that team's designated requirement.
sign_darwin() {
  local dir="$1" pem="$work/sign-cert.pem" subject team hash pass ca line name
  cert_pem "$sign_cert" "$pem" || die "STAGENT_SIGN_CERT: not a certificate"
  subject="$(openssl x509 -in "$pem" -noout -subject -nameopt RFC2253)"
  case "$subject" in
    *"CN=Developer ID Application: "*) ;;
    *) die "STAGENT_SIGN_CERT is not a Developer ID Application certificate ($subject)" ;;
  esac
  team="$(printf '%s\n' "$subject" | sed -n 's/.*OU=\([A-Z0-9]*\).*/\1/p')"
  [ -n "$team" ] || die "STAGENT_SIGN_CERT has no team (OU): $subject"
  hash="$(openssl x509 -in "$pem" -noout -fingerprint -sha1 | sed 's/.*=//; s/://g')"

  keychain="$work/signing.keychain-db"
  pass="$(openssl rand -hex 16)"
  security create-keychain -p "$pass" "$keychain"
  security set-keychain-settings "$keychain" # no auto-lock
  security unlock-keychain -p "$pass" "$keychain"
  security import "$sign_key" -k "$keychain" -T /usr/bin/codesign >/dev/null
  security import "$pem" -k "$keychain" >/dev/null
  # The intermediates codesign embeds; Apple's roots are the system's.
  for ca in DeveloperIDG2CA DeveloperIDCA; do
    curl -fsSL --proto '=https' -o "$work/$ca.cer" "https://www.apple.com/certificateauthority/$ca.cer" ||
      die "cannot download Apple's $ca intermediate"
    security import "$work/$ca.cer" -k "$keychain" >/dev/null
  done
  security set-key-partition-list -S apple-tool:,apple:,codesign: -s -k "$pass" "$keychain" >/dev/null
  # codesign builds the chain from the search list.
  while IFS= read -r line; do
    line="${line#"${line%%[![:space:]]*}"}" line="${line#\"}" line="${line%\"}"
    [ -z "$line" ] || old_keychains+=("$line")
  done < <(security list-keychains -d user)
  security list-keychains -d user -s "$keychain" "${old_keychains[@]}"

  for name in stagent-darwin-amd64 stagent-darwin-arm64; do
    echo "  sign  $name ($team)"
    codesign --force --sign "$hash" --keychain "$keychain" --identifier "$sign_id" \
      --timestamp "$dir/$name"
    codesign --verify --strict "$dir/$name" || die "$name: the signature does not verify"
    codesign -d -r- "$dir/$name" 2>&1 | grep -q "identifier \"$sign_id\" and anchor apple generic.*certificate leaf\[subject.OU\] = \"\{0,1\}$team" ||
      die "$name: unexpected designated requirement: $(codesign -d -r- "$dir/$name" 2>&1)"
  done
}

# adhoc_form <binary> <out>: the binary with its signature, the Go
# linker's ad-hoc one or a Developer ID one, replaced by the same ad-hoc
# one, so a fresh build and a signed asset compare equal.
adhoc_form() {
  cp "$1" "$2"
  codesign --remove-signature "$2"
  codesign --sign - --identifier "$sign_id" "$2"
}

# same_build <fresh build dir> <dist dir>: whether every binary built again
# equals dist/'s (the darwin ones in adhoc_form).
same_build() {
  local t name ok=0
  for t in $targets; do
    name="$(asset "${t%/*}-${t#*/}")"
    case "$name" in
      stagent-darwin-*)
        adhoc_form "$1/$name" "$work/fresh-$name"
        adhoc_form "$2/$name" "$work/dist-$name"
        cmp -s "$work/fresh-$name" "$work/dist-$name" || { echo "differs: $name" >&2; ok=1; } ;;
      *) cmp -s "$1/$name" "$2/$name" || { echo "differs: $name" >&2; ok=1; } ;;
    esac
  done
  return "$ok"
}

# Refuse what --publish cannot publish before spending time on the build.
if [ "$publish" = 1 ]; then
  command -v gh >/dev/null 2>&1 || die "gh is required for --publish"
  git -C "$here" rev-parse --git-dir >/dev/null 2>&1 || die "--publish needs $here in a git checkout"
  untracked="$(git -C "$here" ls-files --others --exclude-standard)"
  [ -z "$untracked" ] || die "untracked files would be built but not published (add or remove them):
$untracked"
  if gh release view "$tag" --repo "$repo" >/dev/null 2>&1; then
    die "release $tag already exists in $repo"
  fi
fi

rm -rf "$dist"
mkdir -p "$dist"
echo "stagent $version ($("$go" version))"
build "$here" "$dist"
if [ -n "$sign_key" ]; then
  sign_darwin "$dist"
else
  echo "  macOS binaries left ad-hoc signed (STAGENT_SIGN_KEY / STAGENT_SIGN_CERT not set): not for a release"
fi
sums "$dist"

# The public source repository has no app next to it; there SHA256SUMS is
# the whole output.
if [ -f "$app/pubspec.yaml" ]; then
tmp="$(mktemp "${dart_out}.XXXXXX")"
{
  cat <<'EOF'
// GENERATED by stagent/scripts/release.sh — do not edit.
// ignore_for_file: lines_longer_than_80_chars

/// Release of the stagent server binary this app installs.
EOF
  printf "const String stagentVersion = '%s';\n\n" "$version"
  cat <<'EOF'
/// Base URL of the release assets (`<base>/<asset name>`).
const String stagentReleaseBaseUrl =
EOF
  printf "    '%s';\n\n" "$base_url"
  cat <<'EOF'
/// Expected sha256 (lowercase hex) of each release asset, keyed `<os>-<arch>`.
const Map<String, String> stagentSha256 = {
EOF
  for t in $targets; do
    key="${t%/*}-${t#*/}"
    sum="$(awk -v n="$(asset "$key")" '$2 == n { print $1 }' "$dist/SHA256SUMS")"
    printf "  '%s': '%s',\n" "$key" "$sum"
  done
  cat <<'EOF'
};

/// Size in bytes of each release asset, keyed `<os>-<arch>`.
const Map<String, int> stagentSize = {
EOF
  for t in $targets; do
    key="${t%/*}-${t#*/}"
    printf "  '%s': %s,\n" "$key" "$(size "$dist/$(asset "$key")")"
  done
  cat <<'EOF'
};

/// File name of the release asset for [assetKey] (`<os>-<arch>`).
String stagentAssetName(String assetKey) =>
    assetKey.startsWith('windows-') ? 'stagent-$assetKey.exe' : 'stagent-$assetKey';

/// Download URL of the release asset for [assetKey].
String stagentAssetUrl(String assetKey) =>
    '$stagentReleaseBaseUrl/${stagentAssetName(assetKey)}';
EOF
} > "$tmp"
mv "$tmp" "$dart_out"
echo "wrote $dist/SHA256SUMS and $dart_out"
else
echo "wrote $dist/SHA256SUMS"
fi
cat "$dist/SHA256SUMS"

[ "$publish" = 1 ] || exit 0

changes="$(awk -v head="## $version" '$0 == head { on = 1; next } on && /^## / { exit } on' "$here/CHANGELOG.md")"
[ -n "$(printf '%s' "$changes" | tr -d '[:space:]')" ] || die "CHANGELOG.md has no \"## $version\" section (the release notes)"

clone="$work/stagent"
gh repo clone "$repo" "$clone" -- --quiet
branch="$(git -C "$clone" symbolic-ref --short HEAD)"
[ "$branch" = main ] || die "$repo's default branch is $branch, not main"

# The public tree becomes exactly the tracked files that were just built.
git -C "$clone" rm -rq --ignore-unmatch -- .
git -C "$here" ls-files -z -- . ':(exclude)dist' | tar -c -f - -C "$here" --null -T - | tar -x -f - -C "$clone"
for f in README.md LICENSE go.mod cmd/stagent/main.go; do
  [ -f "$clone/$f" ] || die "$f is not among the tracked files of $here"
done
git -C "$clone" add -A
if git -C "$clone" diff --cached --quiet; then
  echo "$repo main already holds this source"
else
  git -C "$clone" commit -q -m "stagent $tag source"
fi
commit="$(git -C "$clone" rev-parse HEAD)"
[ -z "$(git -C "$clone" status --porcelain)" ] || die "the clone of $repo has changes outside the commit"

# The commit must build the very binaries the app pins.
echo "rebuilding from $repo@$commit"
mkdir "$work/dist"
build "$clone" "$work/dist"
same_build "$work/dist" "$dist" || die "the binaries built from $repo@$commit differ from dist/"

remote_tag=""
if [ -n "$(git -C "$clone" ls-remote origin "refs/tags/$tag")" ]; then
  git -C "$clone" fetch -q origin "+refs/tags/$tag:refs/tags/$tag"
  remote_tag="$(git -C "$clone" rev-parse "refs/tags/$tag^{commit}")"
  [ "$remote_tag" = "$commit" ] || die "$repo already has tag $tag at $remote_tag, not at $commit"
fi
notes="$changes

---
stagent $version — server binary for SSH Term. Built from this tag ($commit) with $go_version via scripts/release.sh (reproducible: CGO_ENABLED=0, -trimpath, no VCS stamp, empty build id; the macOS binaries then signed with Developer ID, identifier $sign_id); verify downloads against SHA256SUMS, the macOS ones as README.md says."

if [ "$dry_run" = 1 ]; then
  git -C "$clone" log -1 --stat --format='%H %s' | tail -n 20
  if [ -z "$remote_tag" ]; then
    echo "dry run: would push $commit to $repo main and tag it $tag,"
  else
    echo "dry run: would push $commit to $repo main ($tag already points at it),"
  fi
  echo "then create release $tag with $(ls "$dist" | tr '\n' ' ')"
  echo "notes: $notes"
  exit 0
fi

refs=("HEAD:refs/heads/main")
if [ -z "$remote_tag" ]; then
  git -C "$clone" tag "$tag" "$commit"
  refs+=("refs/tags/$tag")
fi
git -C "$clone" push -q --atomic origin "${refs[@]}"

gh release create "$tag" "$dist"/* \
  --repo "$repo" \
  --verify-tag \
  --target "$commit" \
  --title "stagent $tag" \
  --notes "$notes"

# The release must be the tag and the assets checked above.
git -C "$clone" fetch -q origin "+refs/tags/$tag:refs/tags/$tag"
published="$(git -C "$clone" rev-parse "refs/tags/$tag^{commit}")"
[ "$published" = "$commit" ] || die "tag $tag is at $published after publishing, not at $commit"
want_assets="$(for f in "$dist"/*; do echo "$(basename "$f") $(size "$f")"; done | sort)"
got_assets="$(gh release view "$tag" --repo "$repo" --json assets --jq '.assets[] | "\(.name) \(.size)"' | sort)"
[ "$got_assets" = "$want_assets" ] || die "release $tag assets differ from dist/:
$got_assets"
echo "published $base_url ($repo@$commit)"
