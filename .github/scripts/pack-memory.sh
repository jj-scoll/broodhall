#!/bin/bash
# pack-memory.sh <dir>
# Packs each <dir>/memory_<os>_<arch>.tar into memory_<os>_<arch>.tar.xz (the .tar is removed).
#
# The tars run in parallel, one single-threaded xz each: every tar fits in one -9 block
# (192 MiB), so xz -T0 cannot split a tar across threads and only parallel files use the cores.
# amd64 adds the x86 BCJ filter, which every xz since 5.0 decodes. arm64 stays plain LZMA2: its
# BCJ filter needs xz >= 5.4 to decode, and Ubuntu 22.04 and RHEL 9 ship 5.2.5.
# Measured on the v1.5.3 tars (683 MB), pinned to 4 cores, xz 5.8.4, compression only: 101 s and
# 113.1 MB, against 309 s and 116.1 MB for the old `xz -9e -T0` one tar at a time. -e was
# dropped: it cost 18-29% more time for under 0.7% smaller.
# Any failed xz fails the run (xargs exits 123); no memory_*.tar at all fails too.
set -eu
[ "$#" -eq 1 ] || { echo "usage: pack-memory.sh <dir>" >&2; exit 2; }
# shellcheck disable=SC2016 # $1 is the inner sh's argument, so it must not expand here
printf '%s\0' "$1"/memory_*.tar | xargs -0 -P"$(nproc)" -n1 sh -c '
	case "$1" in
	*_amd64.tar) exec xz -T1 --x86 --lzma2=preset=9 "$1" ;;
	*) exec xz -T1 -9 "$1" ;;
	esac' _
