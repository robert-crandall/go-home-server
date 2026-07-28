#!/usr/bin/env bash
#
# Create a new app from template/. Copies the template, rewrites the app's
# module path and all internal imports, and points it at the foundation as a
# real dependency (dropping the in-repo `replace`).
#
# Usage:
#   scripts/new-app.sh <dest-dir> <module-path> [foundation-local-path]
#
#   <dest-dir>              where to create the app (must not exist)
#   <module-path>          the new Go module path, e.g. github.com/you/my-app
#   [foundation-local-path] optional: path to a local checkout of the foundation
#                          to use via `replace` instead of `go get @latest`.
#                          Handy before the foundation is published/tagged.
#
# Examples:
#   scripts/new-app.sh ../my-app github.com/you/my-app
#   scripts/new-app.sh ../my-app github.com/you/my-app .
set -euo pipefail

usage() {
	echo "usage: $0 <dest-dir> <module-path> [foundation-local-path]" >&2
	exit 1
}
[ "$#" -ge 2 ] || usage

dest="$1"
module="$2"
foundation_path="${3:-}"

repo="$(cd "$(dirname "$0")/.." && pwd)"
old_module="github.com/robert-crandall/example-app"
foundation="github.com/robert-crandall/go-home-server"

# Resolve the optional foundation path to an absolute path now, before we cd
# into the new app (otherwise a relative path resolves against the wrong dir).
foundation_abs=""
if [ -n "$foundation_path" ]; then
	foundation_abs="$(cd "$foundation_path" && pwd)"
fi

if [ -e "$dest" ]; then
	echo "error: $dest already exists" >&2
	exit 1
fi

# Copy the template, excluding build artifacts and local files.
if command -v rsync >/dev/null 2>&1; then
	rsync -a \
		--exclude '/web/node_modules' \
		--exclude '/web/dist' \
		--exclude '/.env' \
		--exclude '/bin' \
		"$repo/template/" "$dest/"
else
	cp -R "$repo/template" "$dest"
	rm -rf "$dest/web/node_modules" "$dest/web/dist" "$dest/bin" "$dest/.env"
fi

# Keep only the fallback index.html and .gitignore in the embed directory
# (strip any stale built assets, including sub-directories).
find "$dest/server/internal/web/dist" -mindepth 1 -depth \
	! -name 'index.html' ! -name '.gitignore' -delete 2>/dev/null || true

# Rewrite the app module path and every internal import.
while IFS= read -r f; do
	perl -pi -e "s{\Q$old_module\E}{$module}g" "$f"
done < <(grep -rl "$old_module" "$dest" --include='*.go' --include='go.mod' 2>/dev/null || true)

# Point the app at the foundation as a proper dependency.
(
	cd "$dest/server"
	go mod edit -dropreplace="$foundation"
	if [ -n "$foundation_abs" ]; then
		go mod edit -replace="$foundation=$foundation_abs"
	else
		go get "$foundation@latest"
	fi
	go mod tidy
)

echo "Created $dest (module: $module)"
echo
echo "Next steps:"
echo "  cd $dest"
echo "  cp .env.example .env"
echo "  make dev-db && make run"
