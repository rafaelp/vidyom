#!/usr/bin/env bash
# Adds a Go dependency under the commit lock, so concurrent agents never race
# on go.mod and go.sum, and commits the change on its own.
#
#   scripts/add-dep.sh module@version "why it is needed"
#
# Deliberately does not run `go mod tidy`: other components' in-progress files
# would make it fail or drop requirements. The integration agent tidies.
set -uo pipefail

if [ $# -ne 2 ]; then
  echo 'usage: scripts/add-dep.sh module@version "why it is needed"' >&2
  exit 2
fi
module=$1
reason=$2

root=$(git rev-parse --show-toplevel) || exit 1
root=$(cd "$root" && pwd -P) || exit 1

# shellcheck source=scripts/lib/commit-lock.sh
. "$root/scripts/lib/commit-lock.sh"

cd "$root" || exit 1
commit_lock_acquire "$root" || exit 1
commit_lock_wait_index

if ! GOFLAGS=-mod=mod go get "$module"; then
  echo "add-dep.sh: go get failed" >&2
  git checkout -- go.mod go.sum
  exit 1
fi
if git diff --quiet -- go.mod go.sum; then
  echo "add-dep.sh: $module is already satisfied; nothing to commit"
  exit 0
fi
git add -- go.mod go.sum || exit 1
git commit -F - -- go.mod go.sum <<MSG || exit 1
Add Go dependency ${module%@*}

$reason

Co-Authored-By: Claude Opus 5.5 <noreply@anthropic.com>
MSG
