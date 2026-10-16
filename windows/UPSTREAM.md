# Tracking upstream

This repository is `nextcloud/server` plus a small set of Windows commits on
`win/<major>` branches. Everything else Windows-specific lives under
`windows/`, which upstream never touches, so it never conflicts.

## Branches and tags

| Ref | Meaning |
|---|---|
| `upstream/stableNN`, `vNN.x.y` | upstream, fetched from `nextcloud/server` |
| `win/NN` | `vNN.x.y` (latest point release) + Windows commits |
| `vNN.x.y-win.N` | released build of `win/NN` |
| `win/canary` | Windows commits rebased weekly onto `upstream/master` |

## One-time GitHub setup (fork)

```sh
gh repo edit mstan/nextcloud-server --default-branch win/35   # scheduled workflows run from the default branch
# upstream's ~60 workflows would otherwise run on our pushes:
gh workflow list -R mstan/nextcloud-server --all --json name,path \
  --jq '.[] | select(.path | test("ncwin-") | not) | .path' |
  xargs -n1 basename | xargs -I{} gh workflow disable {} -R mstan/nextcloud-server
# signing keys (zip of core.key/.crt, windows_integration.key/.crt), base64:
base64 -w0 signing.zip | gh secret set NCWIN_SIGNING_KEYS -R mstan/nextcloud-server
```

The signing root (`resources/codesigning/downstream-root.crt`) is public; its
private key never leaves the maintainer's machine. Leaf keys are what CI holds.

## Point release (e.g. 35.0.2 → 35.0.3)

```sh
git fetch upstream --tags
git rebase --onto v35.0.3 v35.0.2 win/35
# bump windows/build/deps.lock.json nextcloud.version/url/sha256
#   sha256: https://download.nextcloud.com/server/releases/nextcloud-35.0.3.zip.sha256
powershell -File windows/build/assemble.ps1 -Clean
# run windows/tests, then tag v35.0.3-win.1
```

The daily `upstream-track` workflow does this automatically and opens an
issue when the rebase conflicts.

## New major (e.g. 36.0.0)

```sh
git switch -c win/36 v36.0.0
git cherry-pick v35.0.N..win/35 -- # every Windows commit, in order
```

Then bump `windows_integration` `max-version`, the deps lock, and re-run
everything. The weekly canary should already have surfaced conflicts.

## Patch rules

1. Linux behaviour must not change. Guard with `function_exists()` or
   `PHP_OS_FAMILY === 'Windows'`, or use `PathHelper::normalizeOsPath()`,
   which is a no-op off Windows.
2. One concern per commit, subject prefixed `win:`.
3. Prefer changing one line over re-indenting a block: smaller diffs rebase
   cleanly.
4. Neutral fixes (guards, `getmypid()`, separator-agnostic regexes) are
   offered upstream; when one merges, drop it here.

## Patch inventory

| Area | Files | Why |
|---|---|---|
| posix: occ/console owner check | `occ`, `console.php` | no `ext-posix` on Windows; `fileowner()` is always 0 |
| posix: cron owner check | `core/Service/CronService.php` | same |
| posix: job PID | `lib/private/BackgroundJob/JobRuns.php` | `posix_getpid()` fatals every cron run; `getmypid()` is portable |
| posix: crashed-job reaper | `core/BackgroundJobs/CleanupBackgroundJobsJob.php` | `posix_kill(pid, 0)`; Windows uses `tasklist` |
| posix: setup check | `apps/settings/lib/SetupChecks/AppDirsWithDifferentOwner.php` | `posix_getuid()` fatals the admin overview |
| posix: scanner error path | `lib/private/Files/Utils/Scanner.php` | `posix_getpwuid()` |
| OS path helpers | `lib/private/Files/Utils/PathHelper.php` | `normalizeOsPath()`, `isAbsoluteOsPath()` |
| drive-letter data dir | `lib/private/legacy/OC_Util.php` | data dir had to start with `/` |
| Local storage realpath | `lib/private/Files/Storage/Local.php` | `\` from `realpath()` made every path look outside the data dir ("Following symlinks is not allowed") |
| migrations discovery | `lib/private/DB/MigrationService.php` | `/`-only regex over `realpath()` output found zero migrations: install created no tables |
| template web roots | `lib/private/Template/{Resource,JS,CSS}Locator.php` | `findWebRoot()` looped forever at a drive root; `\` paths missed the mapping |
| integrity check paths | `lib/private/IntegrityCheck/Checker.php`, `Iterator/ExcludeFoldersByPathFilterIterator.php` | hash keys and exclusions compared `\` paths with `/` |
| virtual-path `dirname()` | ~100 sites in `lib/`, `core/`, `apps/` → `PathHelper::dirname()` | on Windows `dirname('/x')` is `'\'`: loops climbing to `'/'` (SetupManager, Mount\Manager, UserMountCache, dav Directory) never end; install hung |
| Local storage root mode | `Local::getSourcePath()` | cross-storage moves use `Local(['datadir' => '/'])` with `C:/...` paths |
| Local case conflicts | `Local` mkdir/fopen/touch/file_put_contents/rename/copy | NTFS resolves `a.txt` to an existing `A.txt`: silent overwrite + duplicate cache rows; now `ForbiddenException` (403) |
| sharing violations | `Local::unlink()`, `Local::rename()` | AV/indexer handles block delete/rename briefly; bounded retry |
| junctions | `OCP\Files::rmdirr()`, `Local::rmdir()` | directory links need `rmdir()` on Windows |
| data dir / apps_paths normalisation | `Setup.php`, `OC.php`, setup checks, `JSConfigHelper` | backslashed config values never match `/`-joined paths |
| previews | `Preview/Movie.php`, `Preview/Office.php`, `Preview/Storage/LocalPreviewStorage.php` | `ffmpeg.exe` detection, LibreOffice `file:///C:/` URL + quoting, `\` iterator paths |
| misc | `Log/File.php` (chmod per line), `Type/Detection.php` (`test -f`), `SchemaChecker.php` (regex), `PreviewCommand.php` | |

| virtual-path `basename()` | ~65 sites → `PathHelper::basename()`; `Local::getSourcePath()` refuses `\` | on Windows `basename('a\b')` is `'b'`: name validation never saw the forbidden `\`, which the OS then treats as a separator |
| overwrite of open files | `Local::rename()` → `replaceFileWindows()` | a deleted-but-open file (sync client, indexer, AV) blocks its name: delete-then-rename lost the old file and failed the new one; now move-aside, move-in, delete, restore on failure |
| defaults | `OC_Util::checkDataDirectoryPermissions`, `Local` | mode-bit check off and `localstorage.case_insensitive` on by default on Windows (explicit config still wins) |
| `Filesystem::mount()` | `lib/private/Files/Filesystem.php` | `setupFS()` without a user leaves the mount manager null |
| object-store URNs | `StorageObjectStore` | `:` in `urn:oid:N` names an NTFS alternate data stream |
| line endings | `JSCombiner`, `JSConfigHelper`, `EMailTemplate` | `PHP_EOL` made generated JS/mail differ per platform |
| integrity signing | `Checker::loadDownstreamCertificate()`, `resources/codesigning/downstream-root.crt` | patched core is re-signed by the downstream root, trusted for exact-CN matches only |
| tests | `tests/lib/TestCase.php` (`linkDirectory()`), `LocalTest`, `Storage`, `LogFactoryTest`, `AppDirsWithDifferentOwnerTest`, new `LocalWindowsSemanticsTest`, `PathHelperOsPathTest` | junction fallback where symlinks need privileges; skips only for cases impossible on NTFS/Windows |

Release-only apps (not in this repo, e.g. `recommendations`, `photos`, `serverinfo`, `logreader`) have a few cosmetic `dirname()`/`'/'` issues; not patched yet.

### How the patch set is verified

- `windows/build/lint_windows.py`: every bare `dirname()`/`basename()`/`posix_*()` in core is routed, guarded or reviewed (`lint-windows-allow.txt`). New upstream code fails the lint until classified.
- `windows/tests/phpunit-windows.sh` + `junit_check.py`: upstream PHPUnit suites for the touched code, on Windows; only `phpunit-known-upstream.txt` failures (which also fail on stock Linux) are tolerated.
- `windows/tests/linux-compare.sh <ref> <tests...>`: the same suites on Linux (Docker) for stock and patched refs; patched must not fail anything stock passes.
- `windows/tests` (Go): integration suite against a running install (WebDAV, case conflicts, reserved names, long paths, chunked upload, direct disk edits).

### Runtime requirements handled outside PHP

- `OPENSSL_CONF` must point at `php/extras/ssl/openssl.cnf`, or `openssl_pkey_new()` fails ("Unable to create the key").
- Caddy `skip_install_trust`: `tls internal` otherwise blocks on Windows trust-store installation.
- `opcache.enable_cli = 0`, `opcache.file_cache_fallback`: Windows shares one OPcache segment per user; a process mapping `php8ts.dll` elsewhere dies with "Opcode handlers are unusable due to ASLR".
- Bundled PostgreSQL runs with a restricted token: `postgres.exe` refuses administrative rights, and setup runs as SYSTEM under the MSI.

### Disproved (do not re-investigate)

- "Install hangs": the first hang was `findWebRoot()` at a drive root, the second `dirname('/x') === '\'`; the remaining slowness was SQLite fsync on a slow disk (CPU idle, WAL growing).
- `files_sharing` `CacheTest::testShareRenameOriginalFileInRecentResults` failing on Windows: stale state in a reused test instance (users with a last-login but no skeleton); passes on a fresh instance.
- `files_trashbin` `StorageTest::testDeleteVersionsOfFileAsRecipient`: flaky upstream (also fails on stock Linux sometimes).
- `ObjectStore\LocalTest` "not allowed to access the database": upstream test-isolation issue when the file runs alone (stock Linux too).
