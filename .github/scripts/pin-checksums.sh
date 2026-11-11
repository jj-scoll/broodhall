#!/bin/bash
# pin-checksums.sh write <checksums.txt> <version> [root]
# pin-checksums.sh check <checksums.txt> [root]
# <checksums.txt> is the sha256 list the release's publish job made from the archives it built.
#   write  puts each downloading plugin's four lines in plugins/<plugin>/checksums.txt under
#          "# version <version>". The release commits those files, so the expected hashes reach
#          a user through the git checkout and the archives through the release: replacing the
#          release's files alone no longer passes the launchers' check (scripts/run.sh).
#   check  fails when a plugins/<plugin>/checksums.txt in the tree names a different sum for an
#          archive than the list does. Publishing such archives would make every launcher of
#          that version refuse them. A tree without the files passes.
# [root] is the repository root (default: the one this script is in).
set -eu
usage() { echo "usage: pin-checksums.sh write <checksums.txt> <version> [root] | check <checksums.txt> [root]" >&2; exit 2; }
[ "$#" -ge 2 ] || usage
mode=$1 list=$2
[ -f "$list" ] || { echo "$list: no such file" >&2; exit 1; }
plugins="guardrails memory"

# lines <plugin>: the plugin's well-formed lines in the list, in a fixed order
lines() { grep -E "^[0-9a-f]{64}  $1_(linux|darwin)_(amd64|arm64)\.tar\.(gz|xz)$" "$list" | LC_ALL=C sort -k2; }

case $mode in
write)
	[ "$#" -ge 3 ] || usage
	version=$3
	root=${4:-$(cd "$(dirname "$0")/../.." && pwd)}
	[[ $version =~ ^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$ ]] || { echo "version '$version' is not X.Y.Z" >&2; exit 1; }
	# Every plugin is checked before any file is written.
	for p in $plugins; do
		n=$(lines "$p" | wc -l)
		[ "$n" -eq 4 ] || { echo "$list: $n well-formed lines for $p, want 4" >&2; exit 1; }
		[ -d "$root/plugins/$p" ] || { echo "$root/plugins/$p: no such directory" >&2; exit 1; }
	done
	for p in $plugins; do
		{
			echo "# version $version"
			echo "# sha256 of this version's release archives. The release writes this file"
			echo "# (.github/scripts/pin-checksums.sh) and scripts/run.sh checks its download against it."
			lines "$p"
		} >"$root/plugins/$p/checksums.txt"
		echo "wrote plugins/$p/checksums.txt"
	done
	;;
check)
	root=${3:-$(cd "$(dirname "$0")/../.." && pwd)}
	rc=0
	for p in $plugins; do
		f=$root/plugins/$p/checksums.txt
		[ -f "$f" ] || continue
		while IFS= read -r line; do
			if ! grep -qxF "$line" "$list"; then
				echo "plugins/$p/checksums.txt pins '$line', and the built archive has another sum" >&2
				rc=1
			fi
		done < <(grep -v '^#' "$f")
	done
	[ "$rc" -eq 0 ] || echo "The archives just built are not the ones this commit pins, so its launchers would refuse them. A pinned release cannot be rebuilt: cut a new release." >&2
	exit $rc
	;;
*) usage ;;
esac
