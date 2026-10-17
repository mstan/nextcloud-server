#!/bin/sh
# Run the upstream PHPUnit suites that cover code the Windows patch set
# touches, one suite per line of output, against the test instance installed
# in the repo (see windows/tests/README.md). Usage: phpunit-windows.sh <php.exe> [junit dir]
set -u
PHP="$1"
OUT="${2:-}"
cd "${NC_ROOT:-$(dirname "$0")/../..}"
SUITES="
tests/lib/Files
tests/lib/IntegrityCheck
tests/lib/DB/MigrationsTest.php
tests/lib/DB/MigrationServiceTest.php
tests/lib/Template
tests/lib/Preview
tests/lib/BackgroundJob
tests/Core/BackgroundJobs
tests/lib/Log
apps/dav/tests/unit/Connector
apps/dav/tests/unit/Upload
apps/files/tests
apps/files_trashbin/tests
apps/files_versions/tests
apps/files_sharing/tests
apps/files_external/tests
apps/settings/tests
windows/app/windows_integration/tests
"
rc=0
for s in $SUITES; do
	[ -e "$s" ] || { echo "SKIP  $s (missing)"; continue; }
	junit=""
	[ -n "$OUT" ] && junit="--log-junit $OUT/$(echo "$s" | tr '/' '_').xml"
	res=$("$PHP" -d memory_limit=4G vendor-bin/phpunit/vendor/bin/phpunit -c tests/phpunit-autotest.xml --no-coverage $junit "$s" 2>&1 | grep -E '^(OK|Tests:|FAILURES|ERRORS)' | tr '\n' ' ')
	echo "$s: $res"
	case "$res" in *FAILURES*|*ERRORS*) rc=1 ;; esac
done
exit $rc
