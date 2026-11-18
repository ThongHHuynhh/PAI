#!/bin/sh
# Copies a private GitHub repo's PAI releases into dir, laid out the way a
# build stamped with release.Base expects: dir/<version>/<file>, and dir/latest/
# holding the newest stable release. Run it on a schedule (docs/download-server.md).
#
#   GH_TOKEN=<read-only token> sync.sh <owner/repo> <dir>
#
# A release is published only once every file matches its SHA256SUMS; a broken
# download is left in dir/.tmp-<version> and retried on the next run.
set -eu
[ $# -eq 2 ] || { echo "usage: GH_TOKEN=... $0 <owner/repo> <dir>" >&2; exit 2; }
repo=$1 dir=$2
mkdir -p "$dir"
exec 9>"$dir/.sync.lock"
flock -n 9 || { echo "another sync is running" >&2; exit 0; }

# Newest first: tag and whether it is a prerelease, drafts left out.
releases=$(gh release list -R "$repo" --limit 100 --json tagName,isDraft,isPrerelease \
  --jq '.[] | select(.isDraft | not) | "\(.tagName) \(.isPrerelease)"')

# One release that fails is reported and skipped, so it never holds back the rest.
failed=0
for tag in $(printf '%s\n' "$releases" | cut -d' ' -f1); do
  case $tag in v[0-9]*) ;; *) echo "skip $tag: not a version tag" >&2; continue ;; esac
  case $tag in *[!A-Za-z0-9.-]*) echo "skip $tag: unexpected characters" >&2; continue ;; esac
  [ ! -f "$dir/$tag/SHA256SUMS" ] || continue
  tmp=$dir/.tmp-$tag
  rm -rf "$tmp"
  if gh release download "$tag" -R "$repo" -D "$tmp" && (cd "$tmp" && sha256sum -c --quiet SHA256SUMS) && mv "$tmp" "$dir/$tag"; then
    echo "added $tag"
  else
    echo "FAILED $tag: left in $tmp" >&2
    failed=1
  fi
done

# latest/ is the newest stable release, as GitHub's releases/latest is.
latest=$(printf '%s\n' "$releases" | awk '$2 == "false" { print $1; exit }')
if [ -n "$latest" ] && [ -d "$dir/$latest" ] && ! cmp -s "$dir/$latest/VERSION" "$dir/latest/VERSION" 2>/dev/null; then
  rm -rf "$dir/.latest-new" "$dir/.latest-old"
  cp -al "$dir/$latest" "$dir/.latest-new"   # hard links: no second copy on disk
  [ ! -d "$dir/latest" ] || mv "$dir/latest" "$dir/.latest-old"
  mv "$dir/.latest-new" "$dir/latest"
  rm -rf "$dir/.latest-old"
  echo "latest is $latest"
fi

exit $failed
