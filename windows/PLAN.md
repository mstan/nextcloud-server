# Nextcloud for Windows: a native Windows service build

## Context
- Nextcloud upstream refuses to support Windows servers ("We do not support windows and never will", nextcloud/server#15669).
- No maintained native Windows distribution exists. WSL2, Docker and VMs keep data off NTFS or make it slow.
- Goal: a native Windows service that runs unmodified-as-possible upstream Nextcloud and keeps up with every release. The only intended difference is that it works on Windows.
- Owner decisions:
  - Audience: the owner's own machines.
  - Files are edited both through Nextcloud and directly on disk.
  - The database is the user's choice.
  - Track the latest stable major release.
  - Support both LAN and public access, with configurable ports.

### Research findings (verified on stable35 / 35.0.2)
- **No code explicitly refuses Windows.** ownCloud removed those checks in 2016.
- **The hard blockers are unguarded `posix_*` calls and Unix path assumptions:**
  - `occ:11` calls `posix_getuid`.
  - `console.php:47` and `core/Service/CronService.php:126` run the posix owner check.
  - `lib/private/BackgroundJob/JobRuns.php:44` calls `posix_getpid`. This kills every cron mode.
  - `core/BackgroundJobs/CleanupBackgroundJobsJob.php:50` calls `posix_kill`.
  - `lib/private/legacy/OC_Util.php:494` contains `$dataDirectory[0] !== '/'`.
  - `lib/private/Files/Storage/Local.php:58-59,559,566` assumes `/` after `realpath`. Every file access then throws "Following symlinks is not allowed".
  - `lib/private/Template/ResourceLocator.php` (`findWebRoot`) and the JS and CSS locators use the result of `realpath()` with backslashes, so the UI's assets break.
  - Other affected spots: `apps/settings/.../AppDirsWithDifferentOwner.php`, `lib/private/Files/Utils/Scanner.php:178`, and hash paths in `lib/private/IntegrityCheck/Checker.php`.
- **Size of the change:** roughly 10–12 core files and about 50–100 lines. Bundled apps such as files_external, dav chunking, versions and trashbin have not been audited yet.
- **Settings that need only config, no code:**
  - `check_data_directory_permissions=false`
  - `localstorage.case_insensitive=true`, which only partly fixes case handling
  - Windows filename rules: the `forbidden_filename_*` keys, the same ones `occ files:windows-compatible-filenames` sets
  - `binary_search_paths`, `upgrade.disable-web=true`
- **Stack:**
  - FrankenPHP ships official Windows builds (v1.13.0, 2026-10-04) that link the official thread-safe PHP 8.5. Nextcloud 35 accepts PHP 8.3 up to (but not including) 8.6.
  - Thread-safe Windows DLLs exist for APCu, imagick and redis.
  - NSSM is effectively unmaintained.
  - notify_push has no Windows server build.
- **Release cadence:** about 3 major releases a year, each supported for about 13 months. Point releases come every 3–6 weeks. The latest are 35.0.2, 34.0.5 and 33.0.10, all released 2026-10-15.
- **Repo layout:**
  - Release tarballs already contain the shipped apps, `3rdparty` and the compiled assets.
  - A git checkout does not contain the shipped apps.
  - The release-assembly tooling is not public.

## Architecture

**One repository:** the owner's GitHub fork `mstan/nextcloud-server`, cloned at `F:\Projects\nextcloud`.
- Upstream code stays as it is, with Windows patches as small commits on top.
- All Windows-only material lives under the new top-level `windows/` directory. It never conflicts during a rebase.

```
F:\Projects\nextcloud  (fork of nextcloud/server)
  <upstream tree> + Windows commits      branch win/35 = v35.0.2 + patches
  windows/
    supervisor/   Go Windows service "ncwin" (golang.org/x/sys/windows/svc)
    app/          Nextcloud app "windows_integration" (occ commands; no core edits)
    build/        fetch, verify and assemble scripts (PowerShell) + Inno Setup script
    templates/    Caddyfile.tmpl, php.ini.tmpl, config.defaults.php, ncwin.toml.example
    tests/        Windows integration suite (Pester + Go + PHP)
    UPSTREAM.md   rebase procedure and patch inventory (one line per patch, with the reason)
```

### 1. Core patch set (`win/NN` branch)
Every patch keeps the existing behaviour on Linux and changes only what Windows needs. Most are guarded with `function_exists` or `PHP_OS_FAMILY === 'Windows'`.
- **posix calls:**
  - Guard `posix_*` in `occ`, `console.php`, `CronService`, `AppDirsWithDifferentOwner` and `Scanner`.
  - Replace `posix_getpid` with `getmypid()` in `JobRuns`.
  - Add a Windows process-alive check to `CleanupBackgroundJobsJob`, using `OpenProcess` through the supervisor's helper or `tasklist`.
- **Paths:**
  - Add one helper, `\OC\Files\Utils\PathHelper::normalizeOsPath()` (`\` → `/`).
  - Apply it after every `realpath()` in `Local.php`, the `ResourceLocator` family and `Checker`.
  - Accept drive-letter absolute paths in `OC_Util::checkDataDirectoryValidity`.
  - Stop `findWebRoot` at a drive root.
- **Case-insensitive volumes:** in `Local` storage create, rename and copy, reject a target whose name differs only in case from an existing sibling. Throw `InvalidPathException`. Without this, uploading `a.txt` silently overwrites `A.txt`.
- **Windows file locking:** in `Local::rename` and `unlink`, retry with a bounded backoff on Windows sharing violations. Antivirus scanners and the Search indexer hold files open.
- **Integrity checking:**
  - Make `Checker` trust a second root certificate for the `core` scope only. App-store apps keep verifying against Nextcloud's root CA.
  - CI signs the patched core with the fork's own key using `occ integrity:sign-core`.
  - This keeps the integrity check working instead of disabling it.
- **Neutral fixes to send upstream:** offer the posix guards, the `getmypid` change and the `realpath` normalization as upstream PRs. Every one that gets merged shrinks the patch set.

### 2. `windows_integration` app (custom_apps; no core edits)
- `occ windows:apply-changes`: a long-running command that reads JSON lines of changed paths on stdin. For each path it calls `\OC\Files\Utils\Scanner` with the correct storage and locking. This avoids starting `occ` once per event.
- `occ windows:selftest`: checks extensions, long-path support, case handling, filename config and the service account's ACLs.

### 3. `ncwin` supervisor (Go, single Windows service)
- **Service account:** runs as `NT SERVICE\ncwin`, a virtual account.
- **Child processes:** all run inside one Job Object, so they die together with the service.
  - **Database.** If MariaDB or PostgreSQL is bundled, it starts first and readiness is checked. It is shut down gracefully, with `mysqladmin shutdown` or `pg_ctl stop -m fast`. SQLite and external databases need no child process.
  - **FrankenPHP in classic (non-worker) mode.** It runs on a Caddyfile generated from `ncwin.toml`:
    - `http_port`, `https_port`, and the bind addresses
    - a LAN site using `tls internal`, with a list of hostnames and IPs
    - a public site with ACME, using the HTTP-01 or TLS-ALPN challenge
    - Nextcloud's well-known redirects, deny rules for `data`, `config` and `.ht*`, the upload size limit, and timeouts
  - **Background jobs.** `php cron.php` runs every 5 minutes, never overlapping. Nextcloud's background mode is set to `cron`.
  - **File watcher.**
    - Uses `ReadDirectoryChangesW` on every `data/<user>/files` folder and every mount configured under external storage "Local".
    - Filters out `.part`, `appdata_*`, `files_versions`, `files_trashbin` and `uploads`.
    - Debounces events, then feeds them to the long-running `occ windows:apply-changes`.
    - If the change buffer overflows, it rescans the affected root completely.
    - A full reconciliation scan runs every night.
- **Observability:** keeps an always-on ring buffer of watcher events, child process lifecycle and cron runs. It can be queried with `ncwin status` and `ncwin events --since`, which read it over a named pipe.
- **CLI:**
  - `ncwin install|uninstall|start|stop|status`
  - `ncwin occ ...` runs as the service account with the correct php.ini
  - `ncwin grant-path D:\Media` grants the service account access to a folder for external storage
  - `ncwin update`, described in section 4

### 4. Build, release and update
- **Assembly (`windows/build/assemble.ps1`):**
  1. Download the official `nextcloud-X.Y.Z.zip` and verify its sha256 and Nextcloud signature.
  2. Apply `git diff vX.Y.Z..win/vX.Y.Z -- . ':!windows'` to it with `git apply --3way` against the tag.
  3. Add the `windows_integration` app and sign core with the fork's key.
  4. Add pinned and hashed FrankenPHP and PHP, the thread-safe PECL extensions (apcu, imagick with ImageMagick DLLs), and optionally portable MariaDB and PostgreSQL.
  5. Add `ncwin.exe`, then build the Inno Setup installer.
- **Installer:**
  - Asks for the database (SQLite, bundled MariaDB, bundled PostgreSQL, or an existing server), the data directory, the ports, the hostnames and an optional public domain.
  - Turns on `LongPathsEnabled`.
  - Sets ACLs: read access to the program directory, modify access to data, config and custom_apps.
  - Runs `occ maintenance:install` and writes `config.php`: APCu local cache, database file locking, Windows filename rules, `case_insensitive`, `check_data_directory_permissions=false`, `upgrade.disable-web=true`, and `apps_paths` with custom_apps outside the code directory.
- **`ncwin update`:**
  1. Downloads the fork's GitHub release and verifies it.
  2. Stops the web server and cron (enables maintenance mode).
  3. Takes a snapshot: copies the code directory and dumps the database.
  4. Swaps in the new code, keeping config, data and custom_apps.
  5. Runs `occ upgrade` and restarts.
  6. Rolls back automatically if any step fails.

### 5. Keeping up with upstream (automated)
- **Daily GitHub Action (`windows/.github` workflow, copied to the root workflows directory):**
  - Detects new upstream `vNN.x.y` tags on the latest major.
  - Runs `git rebase --onto <new tag> <old tag> win/NN`.
  - Runs the full Windows test suite and publishes release `vX.Y.Z-win.N`.
  - If the rebase conflicts, it opens an issue instead.
- **New major release:** create `win/NN+1` from the `.0` tag by rebasing the patch series.
- **Weekly canary:** rebase onto `master` (the next major) so conflicts surface months early.
- **Patch inventory:** `windows/UPSTREAM.md` lists every patch with its file, its reason, and its upstream PR status, if any.

## Tracking
- Create the Beads epic `Meta: nextcloud-windows` under `System: PC` (`beads-k77w`). Use labels `scope:meta`, `system:pc`, `project:nextcloud-windows` and `repo:nextcloud-server`. Copy the label shape from an existing meta epic.
- Each phase below is one issue under that epic. Record the research findings above in the epic notes, including what is unverified.

## Phases (in order)
1. **Fork and skeleton.**
   - `gh repo fork nextcloud/server`, clone to `F:\Projects\nextcloud`, create `win/35` from `v35.0.2`, and lay out the `windows/` tree.
   - Pin FrankenPHP, PHP and the extensions in `windows/build/deps.lock.json`.
2. **Core patches.** Implement section 1, then confirm that `occ maintenance:install` (SQLite) completes on Windows.
3. **Supervisor.** Service, children, Caddyfile generation, cron loop, ring buffer, CLI.
4. **Watcher and app.** `windows:apply-changes` and the watcher, with handling for buffer overflow.
5. **Build and installer.** `assemble.ps1`, Inno Setup, the database choices, ACLs.
6. **Updates and rollback.** `ncwin update`.
7. **CI.** Windows runner, the upstream-tracking action, the canary, signing.
8. **Audit of bundled apps.** files_external (Local, SMB), dav chunking, versions, trashbin, text, and previews (imagick, ffmpeg path). Fix whatever the test suites expose.

## Verification
- **Upstream PHPUnit on Windows:**
  - Run on a `windows-latest` runner and locally, against SQLite, MariaDB and PostgreSQL.
  - Compare with a Linux run of the same commit. Every failure that happens only on Windows must either get a fix or be justified in UPSTREAM.md.
- **Linux safety check:** run the upstream Linux CI subset (unit tests and php-cs) on `win/NN`. This proves the patches leave Linux behaviour unchanged.
- **Integration suite (`windows/tests`), run against the installed service:**
  - Install with each database choice.
  - The `litmus` WebDAV suite, run from WSL.
  - A chunked upload of more than 10 GB, plus move, rename, delete, trash restore and version restore.
  - Case-collision upload is rejected.
  - Reserved names (`CON`, a name with a trailing dot) are rejected.
  - A path longer than 260 characters works.
  - A file edited in Explorer shows up in PROPFIND within 5 s. The watcher overflow test (10k files created at once) still ends up consistent.
  - An external Local mount of a folder granted with `grant-path` works.
  - Nextcloud desktop client sync, round trip.
  - `occ integrity:check-core` is clean, using the fork's root certificate.
  - The admin overview's setup checks report no errors.
  - Service lifecycle: stop and start, kill FrankenPHP to confirm the supervisor restarts it, and a reboot.
  - `ncwin update` from the previous release, including a forced failure to confirm the rollback.
- **Rebase drill:** replay the patch set from 34.0.5 to 35.0.2 to prove the upstream-tracking procedure works before relying on it.

## Open items to settle during implementation (not blocking)
- Public sites on ports other than 80/443 with no forwarding need the ACME DNS challenge. That means a custom FrankenPHP build (xcaddy) with the owner's DNS provider module.
- Whether PHP's Windows file opens include `FILE_SHARE_DELETE`. Tests decide whether the retry patch is enough.
- notify_push and a Redis-compatible cache (Garnet or Memurai) are out of scope unless the owner asks. Clients fall back to polling.
