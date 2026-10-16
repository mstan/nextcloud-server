# Nextcloud for Windows (ncwin)

> **Personal project, no commitments.** I forked Nextcloud and built this for
> my own use: running Nextcloud natively on Windows as a service. Others are
> free to use it and fork it. I make no commitments to fixing bugs or
> answering issues. Bugs that are not Windows-specific belong upstream at
> [nextcloud/server](https://github.com/nextcloud/server); please report them
> there.

This is [Nextcloud server](https://github.com/nextcloud/server) plus a small
set of patches that make it run natively on Windows (PHP for Windows, NTFS),
and `ncwin`: a Windows service that runs the web server (FrankenPHP), PHP,
an optional bundled database (PostgreSQL or MariaDB), background jobs, and a
watcher that picks up files changed directly on disk.

- Install: the `.msi` from [Releases](https://github.com/mstan/nextcloud-server/releases). See [windows/README.md](../windows/README.md).
- How the fork follows upstream, the patch inventory, and what is known to be open: [windows/UPSTREAM.md](../windows/UPSTREAM.md).
- `master` mirrors the current `win/<major>` branch (Nextcloud 35 at the moment).

Upstream's own README: [README.md](../README.md). Nextcloud is licensed
AGPL-3.0; so is this fork.
