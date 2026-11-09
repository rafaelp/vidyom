#!/usr/bin/env bash
# Shared commit tool for agents working in the same working tree.
#
#   scripts/commit.sh -m "message" -- path...
#   scripts/commit.sh -F message-file -- path...      (-F - reads stdin)
#
# Takes the .commit.lock file lock (retries for up to 15 minutes), stages the
# given paths and commits ONLY those paths, leaving other staged or modified
# files alone. Hooks run normally. The lock is always released.
set -uo pipefail

usage() {
  echo 'usage: scripts/commit.sh (-m "message" | -F file) -- path...' >&2
  exit 2
}

message=""
have_message=0
have_file=0
while [ $# -gt 0 ]; do
  case "$1" in
  -m)
    [ $# -ge 2 ] || usage
    [ "$have_file" -eq 0 ] || usage
    if [ "$have_message" -eq 1 ]; then
      message="$message

$2"
    else
      message="$2"
    fi
    have_message=1
    shift 2
    ;;
  -F)
    [ $# -ge 2 ] || usage
    [ "$have_message" -eq 0 ] || usage
    if [ "$2" = "-" ]; then
      message=$(cat) || exit 1
    else
      message=$(cat -- "$2") || exit 1
    fi
    have_file=1
    shift 2
    ;;
  --)
    shift
    break
    ;;
  *) usage ;;
  esac
done

[ "$have_message" -eq 1 ] || [ "$have_file" -eq 1 ] || usage
[ -n "$message" ] || {
  echo "commit.sh: empty commit message" >&2
  exit 2
}
[ $# -gt 0 ] || usage

cwd=$(pwd -P)
root=$(git rev-parse --show-toplevel) || exit 1
root=$(cd "$root" && pwd -P) || exit 1

# normalize <path>: prints the path relative to the repository root, or fails.
normalize() {
  local input=$1 abs part dir
  case "$input" in
  /*) abs=$input ;;
  *) abs="$cwd/$input" ;;
  esac

  # Lexically resolve "." and ".." without following symlinks.
  local out="" old_ifs=$IFS
  set -f
  IFS=/
  # shellcheck disable=SC2086
  set -- $abs
  IFS=$old_ifs
  set +f
  for part in "$@"; do
    case "$part" in
    "" | .) ;;
    ..) out=${out%/*} ;;
    *) out="$out/$part" ;;
    esac
  done
  abs=${out:-/}

  # Canonicalize the parent so /tmp and /private/tmp style aliases compare equal.
  if [ "$abs" != "/" ] && dir=$(cd "${abs%/*}/" 2>/dev/null && pwd -P); then
    abs="${dir%/}/${abs##*/}"
  fi
  if [ -d "$abs" ]; then
    abs=$(cd "$abs" && pwd -P)
  fi

  if [ "$abs" = "$root" ]; then
    echo "commit.sh: refusing to commit the repository root ('$input'); list explicit paths" >&2
    return 1
  fi
  case "$abs" in
  "$root"/*) ;;
  *)
    echo "commit.sh: path is outside the repository: '$input'" >&2
    return 1
    ;;
  esac

  local rel=${abs#"$root"/}
  case "$rel" in
  .commit.lock)
    echo "commit.sh: refusing to commit .commit.lock" >&2
    return 1
    ;;
  .git | .git/*)
    echo "commit.sh: refusing to commit inside .git" >&2
    return 1
    ;;
  esac
  printf '%s\n' "$rel"
}

paths=()
for arg in "$@"; do
  case "$arg" in
  . | ./)
    echo "commit.sh: refusing '$arg'; list explicit paths" >&2
    exit 2
    ;;
  esac
  rel=$(normalize "$arg") || exit 2
  paths+=("$rel")
done

# shellcheck source=scripts/lib/commit-lock.sh
. "$root/scripts/lib/commit-lock.sh"

cd "$root" || exit 1
commit_lock_acquire "$root" || exit 1
commit_lock_wait_index

# --literal-pathspecs: globs and pathspec magic in a path never widen the set.
if ! git --literal-pathspecs add -- "${paths[@]}"; then
  echo "commit.sh: git add failed" >&2
  exit 1
fi
if ! printf '%s\n' "$message" | git --literal-pathspecs commit -F - -- "${paths[@]}"; then
  echo "commit.sh: git commit failed" >&2
  exit 1
fi
