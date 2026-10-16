#!/bin/sh
# Run PHPUnit tests on Linux (Docker) against a git ref of this repo, to tell
# Windows-only failures from upstream ones and to prove Linux is unaffected.
#   linux-compare.sh <git ref> <test path>...
# Needs the image built from windows/tests/linux/Dockerfile (tag ncwin-linux-test).
set -eu
ref="$1"; shift
# Windows-style path under Git Bash/MSYS: MSYS_NO_PATHCONV is set below.
repo="$(cd "$(dirname "$0")/../.." && (pwd -W 2>/dev/null || pwd))"
name="cmp-$(echo "$ref" | tr '/:' '__')"
export MSYS_NO_PATHCONV=1
docker volume create ncwin-src >/dev/null
git -C "$repo" archive "$ref" | docker run --rm -i -v ncwin-src:/src ncwin-linux-test \
	sh -c "rm -rf /src/$name && mkdir -p /src/$name && tar -x -C /src/$name"
git -C "$repo/3rdparty" archive HEAD | docker run --rm -i -v ncwin-src:/src ncwin-linux-test \
	sh -c "tar -x -C /src/$name/3rdparty"
docker run --rm -v ncwin-src:/src ncwin-linux-test sh -c "
	set -e
	cd /src/$name
	(cd vendor-bin/phpunit && composer install -q --no-interaction)
	php occ maintenance:install --database=sqlite --admin-user=admin --admin-pass=admin --data-dir=/tmp/data >/dev/null
	php tests/enable_all.php >/dev/null
	for t in $*; do
		printf '%s: ' \"\$t\"
		php -d memory_limit=4G vendor-bin/phpunit/vendor/bin/phpunit -c tests/phpunit-autotest.xml --no-coverage \"\$t\" 2>&1 | grep -E '^(OK|Tests:|FAILURES|ERRORS)' | tr '\n' ' '
		echo
	done"
