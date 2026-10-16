# ncwin: Nextcloud server as a native Windows service

Upstream Nextcloud does not support Windows servers. This fork carries the
small set of core patches needed to run it natively on Windows and NTFS, plus
the pieces that make it a service:

| Path | What |
|---|---|
| `supervisor/` | `ncwin.exe` (Go): Windows service that runs the bundled database, FrankenPHP (web + PHP), background jobs and the file watcher |
| `app/windows_integration/` | Nextcloud app: `occ windows:apply-changes`, `windows:watch-roots`, `windows:selftest` |
| `build/` | `assemble.ps1` (release zip + fork patches + pinned runtime → install tree), `deps.lock.json` |
| `tests/` | Windows integration tests |
| `UPSTREAM.md` | how the fork tracks upstream; patch inventory |

## Runtime layout

```
C:\Program Files\ncwin\      install_dir (read-only; replaced by updates)
  ncwin.exe
  php\                       FrankenPHP + PHP 8.5 TS + extensions
  nextcloud\                 patched Nextcloud web root
  mariadb\ | pgsql\          optional bundled database
C:\ProgramData\ncwin\        state_dir
  ncwin.toml                 the only file you edit
  config\config.php          Nextcloud config (NEXTCLOUD_CONFIG_DIR)
  custom_apps\               app-store apps
  generated\                 php.ini, Caddyfile, my.ini (rendered from ncwin.toml)
  log\  tmp\  caddy\  db\
D:\NextcloudData\            data_dir
```

## Commands

```
ncwin setup --config C:\ProgramData\ncwin\ncwin.toml   # first-time provisioning
ncwin install --config ...   # register the service (NT SERVICE\ncwin)
ncwin start | stop | uninstall
ncwin status                 # children, last cron run, ring-buffer bounds
ncwin events --since 10m --kind watch.,proc.
ncwin occ <args>             # occ, run by the service as its own account
ncwin grant-path D:\Media    # let the service use a folder as Local external storage
```

## Direct disk edits

Files changed on disk (Explorer, other apps) are picked up by the watcher:
`ReadDirectoryChangesW` on the data directory and every Local external
mount → debounced batches → `occ windows:apply-changes` (long-running) →
Nextcloud's scanner, which propagates etags so clients sync. A watcher buffer
overflow, a watch that had to be re-established, or service start triggers a
full rescan of the affected root; a nightly reconciliation scan catches the
rest. Every raw event is kept in the ring buffer (`ncwin events --kind watch.`).
