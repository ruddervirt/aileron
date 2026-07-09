#!/usr/bin/env bash
#
# tag.sh — bump the latest semver tag and push it.
#
# Usage:
#   hack/tag.sh --patch                 # v0.0.16 -> v0.0.17
#   hack/tag.sh --minor                 # v0.0.16 -> v0.1.0
#   hack/tag.sh --major                 # v0.0.16 -> v1.0.0
#   hack/tag.sh --patch --notes guacdev # v0.0.16 -> v0.0.17-guacdev
#   hack/tag.sh --patch --dry-run       # show what would happen, don't tag/push
#
set -euo pipefail

bump=""
notes=""
dry_run=false

while [[ $# -gt 0 ]]; do
  case "$1" in
    --major) bump="major"; shift ;;
    --minor) bump="minor"; shift ;;
    --patch) bump="patch"; shift ;;
    --notes) notes="${2:-}"; shift 2 ;;
    --notes=*) notes="${1#*=}"; shift ;;
    --dry-run) dry_run=true; shift ;;
    -h|--help)
      grep '^#' "$0" | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *)
      echo "error: unknown argument: $1" >&2
      exit 1 ;;
  esac
done

if [[ -z "$bump" ]]; then
  echo "error: one of --major, --minor, or --patch is required" >&2
  exit 1
fi

# Refuse to tag a dirty working tree — the tag should point at a clean commit.
if [[ -n "$(git status --porcelain)" ]]; then
  echo "error: working tree is dirty; commit or stash changes before tagging" >&2
  git status --short >&2
  exit 1
fi

# Fetch all tags from the remote so we bump from the true latest.
echo "Fetching tags..." >&2
git fetch --tags --quiet

# Latest tag by semver order, stripping any -notes suffix to get the base version.
latest="$(git tag --sort=-v:refname | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+' | head -1 || true)"
if [[ -z "$latest" ]]; then
  echo "error: no existing vX.Y.Z tags found" >&2
  exit 1
fi

base="${latest%%-*}"          # v0.0.6-guacdev -> v0.0.6
read -r major minor patch <<<"$(echo "${base#v}" | tr '.' ' ')"

case "$bump" in
  major) major=$((major + 1)); minor=0; patch=0 ;;
  minor) minor=$((minor + 1)); patch=0 ;;
  patch) patch=$((patch + 1)) ;;
esac

new="v${major}.${minor}.${patch}"
[[ -n "$notes" ]] && new="${new}-${notes}"

if git rev-parse -q --verify "refs/tags/${new}" >/dev/null; then
  echo "error: tag ${new} already exists" >&2
  exit 1
fi

echo "Latest tag: ${latest}"
echo "New tag:    ${new}"

if $dry_run; then
  echo "(dry run — not tagging or pushing)"
  exit 0
fi

msg="${new}"
[[ -n "$notes" ]] && msg="${new}: ${notes}"

git tag -a "$new" -m "$msg"
git push origin "$new"

echo "Created and pushed ${new}"
