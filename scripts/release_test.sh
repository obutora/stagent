#!/usr/bin/env bash
# Test of release.sh's Windows signing with a throwaway self-signed
# code-signing certificate (openssl): the release's own certificate never
# enters a test. In a copy of this module (no app next to it, so nothing
# outside is rewritten) it runs release.sh signing the Windows exes, then
# checks that
#   1. both exes carry a signature that verifies against the test
#      certificate, with an RFC 3161 timestamp, and dist/<exe>.sig exists;
#   2. SHA256SUMS lists the signed bytes;
#   3. a fresh build with dist/<exe>.sig attached (jsign --detached, the
#      README's check) equals the signed exe byte for byte;
#   4. same_build passes for a fresh build against the signed dist/, and
#      fails once a signed exe's code is changed;
#   5. a tampered signed exe no longer verifies.
# Needs go, openssl, jsign, osslsigncode and the network (the timestamp).
set -euo pipefail

here="$(cd "$(dirname "$0")/.." && pwd)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT
fail() { echo "release_test: $*" >&2; exit 1; }
for tool in go openssl "${JSIGN:-jsign}" "${OSSLSIGNCODE:-osslsigncode}"; do
  command -v "$tool" >/dev/null 2>&1 || fail "needs $tool on PATH"
done
jsign="${JSIGN:-jsign}"
osslsigncode="${OSSLSIGNCODE:-osslsigncode}"

tree="$work/stagent"
mkdir "$tree"
(cd "$here" && git ls-files -z -- . ':(exclude)dist' 2>/dev/null | tar -c -f - --null -T -) | tar -x -f - -C "$tree" ||
  fail "cannot copy the module's tracked files"
[ -f "$tree/scripts/release.sh" ] || fail "the copy has no scripts/release.sh"
cp "$here/scripts/release.sh" "$tree/scripts/release.sh" # the one on disk

openssl req -x509 -newkey rsa:3072 -nodes -days 2 -subj "/CN=stagent release_test" \
  -addext "extendedKeyUsage=codeSigning" -addext "keyUsage=critical,digitalSignature" \
  -keyout "$work/key.pem" -out "$work/cert.pem" 2>/dev/null
openssl pkcs12 -export -inkey "$work/key.pem" -in "$work/cert.pem" -name test \
  -out "$work/test.p12" -passout pass:release-test

export STAGENT_WIN_SIGN_KEYSTORE="$work/test.p12" STAGENT_WIN_SIGN_STOREPASS=release-test \
  STAGENT_WIN_SIGN_ALIAS=test STAGENT_WIN_SIGN_CAFILE="$work/cert.pem"
unset STAGENT_SIGN_KEY STAGENT_SIGN_CERT
"$tree/scripts/release.sh" >"$work/release.log" 2>&1 || { cat "$work/release.log" >&2; fail "release.sh failed"; }
dist="$tree/dist"

exes="stagent-windows-amd64.exe stagent-windows-arm64.exe"
mkdir "$work/fresh"
for exe in $exes; do
  # 1.
  [ -s "$dist/$exe.sig" ] || fail "no $exe.sig"
  out="$("$osslsigncode" verify -CAfile "$work/cert.pem" -in "$dist/$exe" 2>&1)" || fail "$exe does not verify: $out"
  printf '%s\n' "$out" | grep -q 'Timestamp time:' || fail "$exe has no timestamp: $out"
  # 2.
  want="$(shasum -a 256 "$dist/$exe" | cut -d' ' -f1)"
  grep -qx "$want  $exe" "$dist/SHA256SUMS" || fail "SHA256SUMS does not list the signed $exe"
  # 3.
  arch="${exe#stagent-windows-}" arch="${arch%.exe}"
  (cd "$tree" && CGO_ENABLED=0 GOOS=windows GOARCH="$arch" go build \
    -trimpath -buildvcs=false -ldflags '-s -w -buildid=' -o "$work/fresh/$exe" ./cmd/stagent)
  cmp -s "$work/fresh/$exe" "$dist/$exe" && fail "fresh $exe already equals the signed one"
  cp "$work/fresh/$exe" "$work/attach-$exe"
  cp "$dist/$exe.sig" "$work/attach-$exe.sig"
  "$jsign" --detached "$work/attach-$exe" >/dev/null
  cmp -s "$work/attach-$exe" "$dist/$exe" || fail "fresh $exe with $exe.sig attached differs from the signed one"
done

# 4. same_build, from release.sh sourced, on the Windows targets only (the
# darwin comparison needs codesign).
same() {
  local fresh="$1" signed="$2"
  set -- # release.sh reads its own arguments
  (cd "$tree" && source scripts/release.sh && targets="windows/amd64 windows/arm64" && same_build "$fresh" "$signed")
}
same "$work/fresh" "$dist" || fail "same_build fails for a fresh build against the signed dist/"
cp -R "$dist" "$work/tampered"
printf 'X' | dd of="$work/tampered/stagent-windows-amd64.exe" bs=1 seek=4096 conv=notrunc 2>/dev/null
same "$work/fresh" "$work/tampered" 2>/dev/null && fail "same_build passes for a changed signed exe"

# 5.
"$osslsigncode" verify -CAfile "$work/cert.pem" -in "$work/tampered/stagent-windows-amd64.exe" >/dev/null 2>&1 &&
  fail "a tampered exe still verifies"

echo "release_test: ok"
