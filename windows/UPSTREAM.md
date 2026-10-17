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

`master` on the fork mirrors the current `win/NN` and is the default branch
(scheduled workflows run from the default branch). The tracking workflow
updates both; for a new major, `git push --force origin win/NN:master`.

```sh
gh api -X PUT repos/mstan/nextcloud-server/actions/permissions -F enabled=true -f allowed_actions=all
# Deploy key for the bots' pushes (GITHUB_TOKEN may not push workflow-file changes):
ssh-keygen -t ed25519 -N "" -C ncwin-bot -f ncwin-deploy
gh repo deploy-key add ncwin-deploy.pub -R mstan/nextcloud-server --allow-write -t ncwin-bot
gh secret set NCWIN_DEPLOY_KEY -R mstan/nextcloud-server < ncwin-deploy && rm ncwin-deploy ncwin-deploy.pub
# Upstream's own workflows register from the default branch; keep only ncwin-*:
gh api --paginate repos/mstan/nextcloud-server/actions/workflows --jq '.workflows[] | select(.path | test("ncwin-") | not) | .id' |
  xargs -I{} gh api -X PUT repos/mstan/nextcloud-server/actions/workflows/{}/disable
# Optional, for signed CI builds (zip of core.key/.crt, windows_integration.key/.crt):
#   base64 -w0 signing.zip | gh secret set NCWIN_SIGNING_KEYS -R mstan/nextcloud-server
```

Without `NCWIN_SIGNING_KEYS`, CI builds are unsigned (`-NoSign`); build signed
releases locally with `assemble.ps1`. Actions minutes are free for public repos.

## Pinned dependencies

`windows/build/deps.lock.json` pins every third-party download by SHA-256.
Every one is mirrored as an asset of the `deps` pre-release on this repo, with
the original in `upstream_url` (and its hash in the release's `SHA256SUMS`):
upstream hosts rate-limit GitHub's shared runner addresses (HTTP 429), and
FrankenPHP re-uploads its Windows zip daily. The upstream-tracking workflow
mirrors each new Nextcloud release itself after checking its published
SHA-256; `assemble.ps1` retries failed downloads with backoff. To update one: download the new build, compare it file by
file with the current one (which binaries changed, and why), test, then upload
it to `deps` under a versioned, hash-suffixed name and update the lock.

## Point release (e.g. 35.0.2 → 35.0.3)

```sh
git fetch upstream --tags
git rebase --onto v35.0.3 v35.0.2 win/35
# mirror the release zip on the deps release and pin it (see "Pinned dependencies"):
#   nextcloud.version, url (the deps copy), sha256, upstream_url in windows/build/deps.lock.json
#   sha256: https://download.nextcloud.com/server/releases/nextcloud-35.0.3.zip.sha256
powershell -File windows/build/release.ps1 -WinBuild 1            # build and run every gate
powershell -File windows/build/release.ps1 -WinBuild 1 -Publish   # then tag, push and release
```

`release.ps1` runs the same gates as CI (Go tests, Windows lint, `php -l`,
upstream PHPUnit, the integration suite on the signed build) and builds the
MSI, zip and checksums; `-Publish` creates the GitHub release that installed
servers' daily update task installs. A rebuild of the same Nextcloud
version takes the next `-WinBuild`.

The GitHub workflows (`ncwin CI`, `upstream tracking`, `canary`) can do this
automatically but are disabled for now; releases are built by hand.

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
| realpath spelling | `PathHelper::realOsPath()`, `realOsPathOrSelf()`; used by `Local` (constructor, `getSourcePath()`) and `{Resource,JS,CSS}Locator` | `realpath()` keeps the 8.3 spelling (`RUNNER~1`) stored in a symlink/junction target but returns long names for a plain path, so a path through a link failed the data-dir prefix check ("Following symlinks is not allowed") and an app-path symlink resolved to another spelling. Resolving the result again settles on the long form; a data dir that does not exist yet is resolved through its nearest existing ancestor |
| stale realpath cache | `Local::fopen()`, `Local::clearRealpathCache()` | on Windows `file_exists()` also reads the realpath cache, keyed by the caller's spelling (`/` vs `\`): a path another process replaced stayed "missing"; a read-mode miss now clears both spellings and rechecks |
| migrations discovery | `lib/private/DB/MigrationService.php` | `/`-only regex over `realpath()` output found zero migrations: install created no tables |
| template web roots | `lib/private/Template/{Resource,JS,CSS}Locator.php` | `findWebRoot()` looped forever at a drive root; `\` paths missed the mapping |
| integrity check paths | `lib/private/IntegrityCheck/Checker.php`, `Iterator/ExcludeFoldersByPathFilterIterator.php` | hash keys and exclusions compared `\` paths with `/` |
| virtual-path `dirname()` | ~100 sites in `lib/`, `core/`, `apps/` → `PathHelper::dirname()` | on Windows `dirname('/x')` is `'\'`: loops climbing to `'/'` (SetupManager, Mount\Manager, UserMountCache, dav Directory) never end; install hung |
| Local storage root mode | `Local::getSourcePath()` | cross-storage moves use `Local(['datadir' => '/'])` with `C:/...` paths |
| Local case conflicts | `Local` mkdir/fopen/touch/file_put_contents/rename/copy | NTFS resolves `a.txt` to an existing `A.txt`: silent overwrite + duplicate cache rows; now `ForbiddenException` (403) |
| sharing violations | `Local::unlink()`, `Local::rename()` | AV/indexer handles block delete/rename briefly; bounded retry |
| junctions | `OCP\Files::rmdirr()`, `Local::rmdir()` | directory links need `rmdir()` on Windows; `is_dir()` is false for a junction, so `rmdirr()` falls back from `unlink()`; it also clears the realpath cache first (stale entries made directory listings fail) |
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
| tests | `tests/lib/TestCase.php` (`linkDirectory()`), `LocalTest` (out-of-process path swap without `rm`/`mkdir -p`), `{CSS,JS}ResourceLocatorTest` (expected root in long `/` form), `Storage`, `LogFactoryTest`, `AppDirsWithDifferentOwnerTest`, new `LocalWindowsSemanticsTest`, `PathHelperOsPathTest` | junction fallback where symlinks need privileges; skips only for cases impossible on NTFS/Windows |

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

- "Symlink tests fail because `symlink()` with a relative target or `exec('mkdir -p')` misbehave": a relative target fails on Windows (`linkDirectory()` falls back to a junction); `cmd.exe` resolves `mkdir` to its built-in, which has no `-p` (`rm` and `printf` resolve to Git's, which is why only the `mkdir -p` step of the stale-cache tests broke).
- "Install hangs": the first hang was `findWebRoot()` at a drive root, the second `dirname('/x') === '\'`; the remaining slowness was SQLite fsync on a slow disk (CPU idle, WAL growing).

### Open

- litmus `copymove` `begin` intermittently gets `405` creating `litmus/` right after `basic` deleted it (MariaDB instance, 2 of ~8 full runs). Both failures overlapped a full PHPUnit run saturating the same disk; 4 later runs on an idle machine and 35 rounds of `TestDeleteThenRecreate` (delete a populated folder, recreate immediately) never failed. Hypothesis, unconfirmed: under load the DELETE's move to the trash bin hits an open handle (antivirus/watcher scan), falls back to copy+delete, and the pending delete keeps the folder visible to the cache-miss rescan behind `MKCOL`'s existence check. Next step: record Local storage rename fallbacks and trash moves in the ring, then rerun litmus under deliberate disk load.
- `files_sharing` `CacheTest::testShareRenameOriginalFileInRecentResults` fails intermittently on Windows (recipient's recent files lack the skeleton `welcome.txt`). Seen in 2 of 3 full runs (`phpunit-windows.sh`), never when the suite or file runs alone, never on Linux. Not stale state: it failed on a fresh instance, where user3 had `lastLogin = 0` and no home. Instrumented `copySkeleton()` in a passing run shows it is called with the skeleton found and free space > 0. Next step: instrument a *failing* run (likely an exception swallowed by `UserFirstTimeLoggedInListener` or an ordering race with the share mount).
- `files_trashbin` `StorageTest::testDeleteVersionsOfFileAsRecipient`: flaky upstream (also fails on stock Linux sometimes).
- `ObjectStore\LocalTest` "not allowed to access the database": upstream test-isolation issue when the file runs alone (stock Linux too).
- `OCA\Theming\Jobs\MigrateBackgroundImages` logging `NotFoundException /appdata_*/theming/global` on every cron run of a fresh install: upstream (reproduced on stock v35.0.2 Linux after `maintenance:repair --include-expensive`).
