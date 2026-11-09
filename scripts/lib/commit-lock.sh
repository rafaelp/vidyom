# shellcheck shell=bash
# Commit lock helpers shared by scripts/commit.sh and scripts/add-dep.sh.
#
# The lock is the file .commit.lock at the repository root, created atomically
# with noclobber. Other sessions use the same convention, so its contents are
# never read and a held lock is never broken: we wait, then give up.

commit_lock_file=""
commit_lock_held=0

commit_lock_release() {
  if [ "$commit_lock_held" -eq 1 ]; then
    rm -f "$commit_lock_file"
    commit_lock_held=0
  fi
}

# commit_lock_acquire <repo-root>: waits up to 15 minutes.
commit_lock_acquire() {
  commit_lock_file="$1/.commit.lock"
  trap commit_lock_release EXIT
  trap 'commit_lock_release; exit 129' HUP
  trap 'commit_lock_release; exit 130' INT
  trap 'commit_lock_release; exit 143' TERM

  local deadline=$(($(date +%s) + 900))
  while :; do
    if (
      set -C
      : >"$commit_lock_file"
    ) 2>/dev/null; then
      commit_lock_held=1
      return 0
    fi
    if [ "$(date +%s)" -ge "$deadline" ]; then
      echo "commit lock: could not acquire $commit_lock_file within 15 minutes" >&2
      return 1
    fi
    sleep 2
  done
}

# commit_lock_wait_index: another process may hold git's own index lock
# (a session not using our lock file). Wait briefly instead of failing.
commit_lock_wait_index() {
  local index_lock waited=0
  index_lock=$(git rev-parse --git-path index.lock)
  while [ -e "$index_lock" ] && [ "$waited" -lt 60 ]; do
    sleep 1
    waited=$((waited + 1))
  done
}
