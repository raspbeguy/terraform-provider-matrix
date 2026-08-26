#!/usr/bin/env sh
#
# Fails when a published version constraint has fallen behind the latest release
# tag.
#
# The constraint in examples/provider/provider.tf is copied into docs/index.md,
# which is the first page the registry shows. Nothing bumps it and nothing used
# to check it, so it sat at "~> 0.1" while the provider reached v0.4.0. Anyone
# who copied it got a provider four releases old.
#
# It searches for the pattern rather than reading a list of files, so an example
# added later is covered without anyone remembering to add it here.
#
# Only a constraint that has fallen *behind* is a defect. One that is ahead is a
# release being prepared: the bump and the tag cannot land in the same instant,
# and the tag should point at a tree whose example already names the version it
# ships. So the release order is bump, merge, tag, and nothing turns red.
set -eu

latest=$(git tag --list 'v[0-9]*' --sort=-v:refname | head -n 1)
if [ -z "$latest" ]; then
	echo "no release tag yet, so there is nothing to compare against"
	exit 0
fi

# v0.4.0 gives 0.4, because a constraint names the minor line.
want=$(printf '%s' "${latest#v}" | cut -d. -f1,2)

# behind reports whether the first minor line comes before the second.
behind() {
	got_major=${1%%.*}
	got_minor=${1##*.}
	want_major=${2%%.*}
	want_minor=${2##*.}
	if [ "$got_major" -lt "$want_major" ]; then
		return 0
	fi
	if [ "$got_major" -eq "$want_major" ] && [ "$got_minor" -lt "$want_minor" ]; then
		return 0
	fi
	return 1
}

pattern='version[[:space:]]*=[[:space:]]*"~>[[:space:]]*[0-9][0-9]*\.[0-9][0-9]*"'
files=$(git grep -l -E "$pattern" -- '*.tf' '*.md' || true)
if [ -z "$files" ]; then
	echo "no version constraint found, which is itself suspicious" >&2
	exit 1
fi

status=0
for file in $files; do
	for got in $(sed -n -E 's/.*version[[:space:]]*=[[:space:]]*"~>[[:space:]]*([0-9]+\.[0-9]+)".*/\1/p' "$file"); do
		if behind "$got" "$want"; then
			echo "$file pins \"~> $got\", which is behind the latest release $latest." >&2
			status=1
		fi
	done
done

if [ "$status" -ne 0 ]; then
	echo >&2
	echo "Set every constraint to \"~> $want\" or later, then run 'make docs' so the" >&2
	echo "generated pages match, and commit both." >&2
	exit 1
fi

echo "no version constraint is behind the latest release ($latest)"
