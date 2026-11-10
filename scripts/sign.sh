#!/usr/bin/env bash
# The maintainer's signatures, one command each, on the laptop that holds
# the offline keys. The keys are read from files and handed to the
# redundanet binary built from main; nothing here sends them anywhere.
#
#   scripts/sign.sh release VERSION   check the draft release agent/vVERSION, build it again
#                                     here from the tag and compare, build the index revision
#                                     that adds it, sign, merge, undraft
#   scripts/sign.sh manifest [PR]     sign the manifest of a pull request, or of the current
#                                     branch, and merge it once CI is green
#
# Options before the mode: --dry-run checks everything and signs nothing;
# --yes skips the one confirmation; --trust-github signs a release without
# building it here, which leaves the signature vouching for whatever the
# build service produced. Keys: RELEASE_KEY and OPERATOR_KEY name
# the files, by default ~/.redundanet/release.key and ~/.redundanet/operator.key.
set -euo pipefail

usage() { sed -n '2,16p' "$0" | sed 's/^# \{0,1\}//'; exit 2; }
die() { echo "sign.sh: $*" >&2; exit 1; }
say() { printf '\n== %s\n' "$*"; }

yes=0 dry=0 trust=0
while [ $# -gt 0 ]; do
  case "$1" in
    --yes) yes=1; shift ;;
    --trust-github) trust=1; shift ;;
    --dry-run) dry=1; shift ;;
    -h|--help) usage ;;
    *) break ;;
  esac
done
mode=${1:-}
[ -n "$mode" ] || usage
shift

RELEASE_KEY=${RELEASE_KEY:-$HOME/.redundanet/release.key}
OPERATOR_KEY=${OPERATOR_KEY:-$HOME/.redundanet/operator.key}

root=$(git rev-parse --show-toplevel 2>/dev/null) || die "run it inside the repository"
cd "$root"
for t in gh go git; do command -v "$t" >/dev/null || die "$t is needed"; done
[ -z "$(git status --porcelain --untracked-files=no)" ] || die "the working tree has changes; commit or set them aside first"
start=$(git branch --show-current)
tmp=$(mktemp -d)
finished=0 made_branch=""
# A run that stops early, a dry run included, leaves the repository as it
# found it: the files this script writes are put back, the branch it made
# goes, and the starting branch is checked out again.
cleanup() {
  rm -rf "$tmp"
  [ "$finished" = 1 ] && return
  git checkout -q -- releases/index.yaml releases/index.sig manifests/manifest.sig 2>/dev/null || true
  if [ -n "$start" ] && [ "$(git branch --show-current)" != "$start" ]; then
    git switch -q "$start" 2>/dev/null || true
  fi
  if [ -n "$made_branch" ] && [ "$made_branch" != "$start" ]; then
    git branch -D "$made_branch" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

confirm() {
  if [ "$dry" = 1 ]; then echo "dry run: stopping before \"$1\", nothing signed"; exit 0; fi
  [ "$yes" = 1 ] && return 0
  printf '%s [y/N] ' "$1"
  read -r answer
  case "$answer" in y|Y|yes) ;; *) die "stopped; nothing signed" ;; esac
}

# Waits for the pull request's checks when it has any; GitHub registers
# them a few seconds after a push.
wait_checks() {
  sleep 10
  if [ "$(gh pr checks "$1" --json name --jq length 2>/dev/null || echo 0)" -gt 0 ]; then
    gh pr checks "$1" --watch --fail-fast || die "the checks of #$1 failed; fix them and merge by hand"
  fi
}

# The signing tool comes from main as it is on GitHub, built before any
# pull request is checked out, so a branch cannot change what signs it.
say "the signing tool, from main"
git fetch -q origin --tags --force
git switch -q main
git merge -q --ff-only origin/main
(cd agent && go build -o "$tmp/rn" ./cmd/redundanet)
"$tmp/rn" version

sign_release() {
  local version=${1:-}
  [ -n "$version" ] || die "usage: sign.sh release VERSION"
  local tag="agent/v$version" branch="release/index-$version"
  [ "$dry" = 1 ] || [ -f "$RELEASE_KEY" ] || die "no release key at $RELEASE_KEY (set RELEASE_KEY)"

  say "the release $tag, checked here on its own"
  local draft
  draft=$(gh release view "$tag" --json isDraft --jq .isDraft) || die "no release $tag"
  [ "$draft" = true ] || die "$tag is already published"
  git merge-base --is-ancestor "$(git rev-parse "refs/tags/$tag^{commit}")" origin/main || die "$tag is not on main"
  local runs
  runs=$(gh run list --workflow agent-release.yml --branch "$tag" --json databaseId --jq length)
  [ "$runs" = 1 ] || die "$runs workflow runs for $tag; its archives may have been replaced, check by hand"
  gh release download "$tag" -D "$tmp/assets" -p '*.tar.gz' -p '*.zip' -p index-fragment.yaml
  gh release view "$tag" --json assets --jq '.assets[] | select(.name|test("\\.(tar\\.gz|zip)$")) | "\(.name) \(.size)"' > "$tmp/sizes"
  while read -r name size; do
    [ "$(wc -c < "$tmp/assets/$name" | tr -d ' ')" = "$size" ] || die "$name: $size bytes on the release, a different file downloaded"
  done < "$tmp/sizes"
  echo "tag on main, one workflow run, $(wc -l < "$tmp/sizes" | tr -d ' ') archives the size the release says"

  # Everything above compares GitHub with GitHub. This is the check that
  # does not: the tag's source, which is on main, is built here with the
  # tag's own scripts/build-agent.sh, and every binary inside an archive
  # must equal the one built here, byte for byte. Only then does the
  # signature say "this source", not "whatever the release holds". The
  # source is a clone, so the build sees a clean tree at the tag as the
  # workflow did.
  if [ "$trust" = 1 ]; then
    echo
    echo "WARNING: --trust-github: the archives are not compared with a build made here."
    echo "The signature will vouch for whatever the build service produced."
  else
    say "the same binaries, built here from $tag"
    git clone -q -c advice.detachedHead=false --branch "$tag" "$root" "$tmp/src"
    [ "$(git -C "$tmp/src" rev-parse HEAD)" = "$(git rev-parse "refs/tags/$tag^{commit}")" ] || die "the clone is not at $tag"
    [ -x "$tmp/src/scripts/build-agent.sh" ] || die "$tag has no scripts/build-agent.sh: it predates the rebuild check; cut a new version, or sign with --trust-github"
    "$tmp/src/scripts/build-agent.sh" "$version" "$tmp/built" >/dev/null
    mkdir "$tmp/unpacked"
    local archive checked=0
    for archive in "$tmp/assets"/*.tar.gz "$tmp/assets"/*.zip; do
      case "$archive" in
        *.zip) unzip -q "$archive" -d "$tmp/unpacked" ;;
        *) tar -xzf "$archive" -C "$tmp/unpacked" ;;
      esac
    done
    local dir name bin
    for dir in "$tmp/built"/redundanet-*/; do
      name=$(basename "$dir")
      bin=redundanet; case "$name" in *-windows-*) bin=redundanet.exe ;; esac
      [ -f "$tmp/unpacked/$name/$bin" ] || die "$name: the release has no such binary"
      [ "$(find "$tmp/unpacked/$name" -type f | wc -l | tr -d ' ')" = 1 ] || die "$name: the archive holds more than the binary"
      cmp -s "$dir$bin" "$tmp/unpacked/$name/$bin" || die "$name: the binary on the release is not the one this source builds. Do not sign. (A binary that was code signed after the build differs too; see releases/README.md.)"
      checked=$((checked + 1))
    done
    [ "$checked" = "$(wc -l < "$tmp/sizes" | tr -d ' ')" ] || die "$checked binaries built here, $(wc -l < "$tmp/sizes" | tr -d ' ') archives on the release"
    echo "$checked binaries on the release equal the ones built here from $tag"
  fi

  say "the index revision, built here on the current main"
  git switch -q -C "$branch" origin/main
  made_branch=$branch
  "$tmp/rn" release add --index releases/index.yaml --fragment "$tmp/assets/index-fragment.yaml" --assets "$tmp/assets"
  if git rev-parse -q --verify "origin/$branch" >/dev/null; then
    if git diff --quiet "origin/$branch" -- releases/index.yaml; then
      echo "identical to the pull request's index"
    else
      echo "note: the pull request's index differs (main moved since it was made); this one replaces it"
    fi
  fi
  local rev
  rev=$(sed -n 's/^revision: //p' releases/index.yaml)
  git --no-pager diff --stat origin/main -- releases/index.yaml

  confirm "sign index revision $rev with $version and publish it?"
  "$tmp/rn" release sign --key "$RELEASE_KEY" --index releases/index.yaml
  git add releases/index.yaml releases/index.sig
  git commit -q -m "releases: index revision $rev, $version"
  git push -q --force-with-lease origin "$branch"
  local pr
  pr=$(gh pr list --head "$branch" --state open --json number --jq '.[0].number // empty')
  if [ -z "$pr" ]; then
    gh pr create --base main --head "$branch" --title "releases: index revision $rev, $version" --body "Signed by scripts/sign.sh." >/dev/null
    pr=$(gh pr view "$branch" --json number --jq .number)
  fi
  wait_checks "$pr"
  gh pr merge "$pr" --merge --delete-branch --match-head-commit "$(git rev-parse HEAD)"
  gh release edit "$tag" --draft=false >/dev/null
  git switch -q main
  git merge -q --ff-only origin/main 2>/dev/null || git pull -q --ff-only
  git branch -D "$branch" >/dev/null 2>&1 || true
  finished=1
  echo "published $version: index revision $rev on main, release undrafted; devices take it within the hour"
}

sign_manifest() {
  local pr=${1:-}
  [ "$dry" = 1 ] || [ -f "$OPERATOR_KEY" ] || die "no operator key at $OPERATOR_KEY (set OPERATOR_KEY)"
  if [ -n "$pr" ]; then
    gh pr checkout "$pr" >/dev/null
  else
    [ "$start" != main ] || die "on main: name the pull request, or run it on the branch that changes the manifest"
    git switch -q "$start"
    pr=$(gh pr list --head "$start" --state open --json number --jq '.[0].number // empty' 2>/dev/null || true)
  fi
  local branch
  branch=$(git branch --show-current)

  say "manifests/manifest.yaml on $branch, against main"
  git --no-pager diff origin/main -- manifests/manifest.yaml
  local new old
  new=$(sed -n 's/^revision: //p' manifests/manifest.yaml)
  old=$(git show origin/main:manifests/manifest.yaml | sed -n 's/^revision: //p')
  [ "$new" -gt "$old" ] || die "revision $new does not rise above main's $old; agents would ignore it"
  "$tmp/rn" validate --manifest manifests/manifest.yaml

  confirm "sign manifest revision $new?"
  "$tmp/rn" manifest sign --key "$OPERATOR_KEY" --manifest manifests/manifest.yaml
  "$tmp/rn" manifest verify --manifest manifests/manifest.yaml
  git add manifests/manifest.sig
  git commit -q -m "manifests: revision $new, signed"
  if [ -n "$pr" ]; then
    git push -q
    wait_checks "$pr"
    gh pr merge "$pr" --merge --delete-branch --match-head-commit "$(git rev-parse HEAD)"
    git switch -q main
    git merge -q --ff-only origin/main 2>/dev/null || git pull -q --ff-only
    finished=1
    echo "manifest revision $new merged; agents apply it within five minutes"
  else
    git push -q -u origin "$branch"
    finished=1
    echo "signed and pushed $branch; open its pull request and merge it, then agents apply it within five minutes"
  fi
}

case "$mode" in
  release) sign_release "$@" ;;
  manifest) sign_manifest "$@" ;;
  *) usage ;;
esac
