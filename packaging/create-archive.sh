#!/bin/bash
# Build peon-<version>.tar.gz from the committed tree, with Go modules
# vendored so the RPM builds offline. Packit runs this as its create-archive
# action and reads the archive path from the last line of output.
set -euo pipefail

version="${PACKIT_PROJECT_VERSION:-$(rpmspec -q --qf '%{version}\n' peon.spec | head -1)}"
name="peon-${version}"
top="$(git rev-parse --show-toplevel)"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

git -C "$top" archive --format=tar --prefix="${name}/" HEAD | tar -x -C "$work"
(cd "$work/$name" && go mod vendor && go mod verify >&2)
tar -C "$work" -czf "$top/${name}.tar.gz" "$name"

echo "${name}.tar.gz"
