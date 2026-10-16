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

Release-only apps (not in this repo, e.g. `recommendations`, `photos`, `serverinfo`, `logreader`) have a few cosmetic `dirname()`/`'/'` issues; not patched yet.

### Runtime requirements handled outside PHP

- `OPENSSL_CONF` must point at `php/extras/ssl/openssl.cnf`, or `openssl_pkey_new()` fails ("Unable to create the key").
- Caddy `skip_install_trust`: `tls internal` otherwise blocks on Windows trust-store installation.
