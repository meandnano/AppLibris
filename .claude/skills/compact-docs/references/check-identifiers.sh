#!/bin/bash
# Prints every backticked identifier in the notes that grep cannot find in
# the source tree. External names (syscalls, inotify flags, htmx patterns)
# are expected non-hits; anything else is stale
cd "$(git rev-parse --show-toplevel)" || exit 1
for f in docs/notes/*.md; do
  grep -oE '`[^`]+`' "$f" | tr -d '`' | sort -u | while IFS= read -r id; do
    case "$id" in *" "*|*/*|*.md|*.sql|*.js|*.html|*.json|*.css|*\<*|*\>*) continue;; esac
    base="${id##*.}"; base="${base%%(*}"; base="${base%%=*}"; base="${base%%:*}"; base="${base%%,*}"
    [ -z "$base" ] && continue
    grep -rqwF -- "$base" internal cmd Dockerfile Makefile go.mod .github 2>/dev/null || echo "$f: $id"
  done
done
