<?php

declare(strict_types=1);

/**
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\WindowsIntegration\Service;

use OC\Files\Cache\Scanner as CacheScanner;
use OC\Files\SetupManager;
use OC\Files\Storage\Home;
use OC\Files\Storage\Local;
use OC\Files\Utils\Scanner;
use OCP\DB\Exception as DbException;
use OCP\EventDispatcher\IEventDispatcher;
use OCP\Files\NotFoundException;
use OCP\Files\Storage\IStorage;
use OCP\IDBConnection;
use OCP\IUserManager;
use OCP\Lock\LockedException;
use Psr\Log\LoggerInterface;

/**
 * Applies a batch of file-system change notifications to the file cache.
 *
 * For every changed path P:
 *  - if P exists: scan P (recursively only when it is a directory listed in
 *    the request's "trees", i.e. one that arrived; otherwise shallowly);
 *  - scan P's parent shallowly, which picks up deletions and the old name of
 *    a rename.
 * A path whose ancestors no longer exist falls back to the nearest existing one.
 *
 * A path the file cache already reflects is skipped before any scan, so
 * Nextcloud's own writes (echoed back by the watcher) never take scan locks:
 * a file is current when its cached storage mtime and size equal the disk's,
 * a directory when its cached child names equal the names on disk.
 */
class ChangeApplier {
	public function __construct(
		private RootMap $roots,
		private IUserManager $userManager,
		private IDBConnection $db,
		private IEventDispatcher $eventDispatcher,
		private LoggerInterface $logger,
		private SetupManager $setupManager,
	) {
	}

	/**
	 * @param list<string> $osPaths
	 * @param list<string> $osTrees subset of $osPaths that arrived (added or renamed in) and may hold unseen descendants
	 * @return array{scanned: int, skipped: int, ignored: int, retry: list<string>, failed: list<string>, errors: list<string>}
	 */
	public function apply(array $osPaths, array $osTrees = []): array {
		$home = [];  // uid => [ncPath => osPath]
		$local = []; // root => [internalPath => osPath]
		$homeTrees = [];
		$localTrees = [];
		$treeSet = array_flip($osTrees);
		$ignored = 0;
		foreach (array_unique($osPaths) as $os) {
			$r = $this->roots->resolve($os);
			if ($r === null) {
				$ignored++;
			} elseif ($r['kind'] === 'home') {
				$home[$r['user']][$r['path']] = $os;
				if (isset($treeSet[$os])) {
					$homeTrees[$r['user']][] = $r['path'];
				}
			} else {
				$local[$r['root']][$r['path']] = $os;
				if (isset($treeSet[$os])) {
					$localTrees[$r['root']][] = $r['path'];
				}
			}
		}

		$result = self::emptyResult($ignored);
		foreach ($home as $uid => $paths) {
			$this->applyHome((string)$uid, $paths, $homeTrees[$uid] ?? [], $result, true);
		}
		foreach ($local as $root => $paths) {
			$this->applyLocal((string)$root, $paths, $localTrees[$root] ?? [], $result, true);
		}
		return $result;
	}

	/**
	 * Full rescan of one watched root (after a watcher buffer overflow, or nightly).
	 * Never skips anything: it is the fallback that repairs whatever the
	 * change-driven path missed.
	 *
	 * @return array{scanned: int, skipped: int, ignored: int, retry: list<string>, failed: list<string>, errors: list<string>}
	 */
	public function rescan(string $osRoot): array {
		$osRoot = RootMap::normalize($osRoot);
		$result = self::emptyResult();
		if ($osRoot === $this->roots->dataDir()) {
			foreach ($this->userManager->getSeenUsers() as $user) {
				$path = '/' . $user->getUID() . '/files';
				$this->applyHome($user->getUID(), [$path => $osRoot . '/' . $user->getUID() . '/files'], [$path], $result, false);
			}
			return $result;
		}
		$r = $this->roots->resolve($osRoot);
		if ($r === null) {
			$result['errors'][] = "not a watched root: $osRoot";
		} elseif ($r['kind'] === 'home') {
			$this->applyHome($r['user'], [$r['path'] => $osRoot], [$r['path']], $result, false);
		} else {
			$this->applyLocal($r['root'], [$r['path'] => $osRoot], [$r['path']], $result, false);
		}
		return $result;
	}

	/**
	 * @return array{scanned: int, skipped: int, ignored: int, retry: list<string>, failed: list<string>, errors: list<string>}
	 */
	private static function emptyResult(int $ignored = 0): array {
		return ['scanned' => 0, 'skipped' => 0, 'ignored' => $ignored, 'retry' => [], 'failed' => [], 'errors' => []];
	}

	/**
	 * Expand changed paths into scan jobs: [path => recursive?]. Parents are
	 * scanned shallowly; a recursive scan of an ancestor subsumes descendants.
	 * Only directories listed in $trees are scanned recursively. A candidate
	 * for which $isCurrent is true is skipped instead of scanned.
	 *
	 * @param list<string> $paths
	 * @param list<string> $trees paths that arrived and may hold unseen descendants
	 * @param callable(string): bool $exists
	 * @param callable(string): bool $isDir
	 * @param callable(string, bool): bool $isCurrent the cache already reflects the path (second argument: it is a directory)
	 * @return array{jobs: array<string, bool>, skipped: int}
	 */
	public static function plan(array $paths, array $trees, string $floor, callable $exists, callable $isDir, callable $isCurrent): array {
		$treeSet = array_flip($trees);
		$candidates = [];
		foreach ($paths as $p) {
			if ($p !== $floor && $exists($p)) {
				$candidates[$p] = ($candidates[$p] ?? false) || (isset($treeSet[$p]) && $isDir($p));
			}
			$parent = self::parentOf($p, $floor);
			while ($parent !== $floor && !$exists($parent)) {
				$parent = self::parentOf($parent, $floor);
			}
			$candidates[$parent] = $candidates[$parent] ?? false;
		}

		$jobs = [];
		$skipped = 0;
		foreach ($candidates as $p => $recursive) {
			$p = (string)$p;
			if ($isCurrent($p, $isDir($p))) {
				$skipped++;
			} else {
				$jobs[$p] = $recursive;
			}
		}

		// Drop jobs covered by a recursive ancestor job.
		$recursiveJobs = array_map(strval(...), array_keys(array_filter($jobs)));
		foreach (array_map(strval(...), array_keys($jobs)) as $p) {
			foreach ($recursiveJobs as $r) {
				if ($p !== $r && ($r === '' || str_starts_with($p . '/', rtrim($r, '/') . '/'))) {
					unset($jobs[$p]);
					break;
				}
			}
		}
		return ['jobs' => $jobs, 'skipped' => $skipped];
	}

	/**
	 * Lock-free check that the file cache already holds $internal as it is on
	 * disk. Files use the test Nextcloud's own scanner applies (same storage
	 * mtime and size); directories compare the exact set of child names, since
	 * a directory's mtime cannot show a change within the same second.
	 * Anything unexpected counts as not current.
	 */
	private function isCurrent(IStorage $storage, string $internal, bool $isDir): bool {
		try {
			$cache = $storage->getCache();
			$entry = $cache->get($internal);
			if ($entry === false || $entry->getSize() === -1) {
				return false;
			}
			if (($entry->getMimeType() === 'httpd/unix-directory') !== $isDir) {
				return false;
			}
			clearstatcache();
			if (!$isDir) {
				$mtime = $storage->filemtime($internal);
				$size = $storage->filesize($internal);
				return $mtime !== false && $size !== false
					&& $entry->getStorageMTime() === (int)$mtime
					&& $entry->getSize() == $size;
			}
			$disk = [];
			foreach ($storage->getDirectoryContent($internal) as $child) {
				$name = (string)$child['name'];
				if (!CacheScanner::isPartialFile($internal . '/' . $name) && ($child['scan_permissions'] ?? $child['permissions']) !== 0) {
					$disk[] = $name;
				}
			}
			$cached = array_map(fn ($e) => $e->getName(), $cache->getFolderContentsById($entry->getId()));
			sort($disk);
			sort($cached);
			return $disk === $cached;
		} catch (\Throwable) {
			return false;
		}
	}

	/**
	 * String-based dirname that never climbs above $floor. (PHP's dirname()
	 * returns "\\" for "/x" on Windows.)
	 */
	private static function parentOf(string $p, string $floor): string {
		$i = strrpos($p, '/');
		$d = $i === false ? '' : substr($p, 0, $i);
		return strlen($d) <= strlen($floor) ? $floor : $d;
	}

	/**
	 * @param array<string, string> $paths ncPath => osPath
	 * @param list<string> $trees ncPaths scanned recursively when they are directories
	 */
	private function applyHome(string $uid, array $paths, array $trees, array &$result, bool $suppress): void {
		$user = $this->userManager->get($uid);
		if ($user === null) {
			$result['ignored'] += count($paths);
			return;
		}
		$floor = '/' . $uid . '/files';
		$toOs = fn (string $nc) => $this->roots->dataDir() . '/' . $uid . '/files' . substr($nc, strlen($floor));
		$storage = $suppress ? new Home(['user' => $user]) : null;
		$plan = self::plan(array_keys($paths), $trees, $floor,
			fn ($p) => file_exists($toOs($p)),
			fn ($p) => is_dir($toOs($p)),
			fn ($p, $dir) => $storage !== null && $this->isCurrent($storage, 'files' . substr($p, strlen($floor)), $dir));
		$result['skipped'] += $plan['skipped'];

		$scanner = new Scanner($user, $this->db, $this->eventDispatcher, $this->logger, $this->setupManager);
		foreach ($plan['jobs'] as $path => $recursive) {
			$path = (string)$path;
			$this->runJob($toOs($path), $result, function () use ($scanner, $path, $recursive): void {
				try {
					$scanner->scan($path, $recursive ? CacheScanner::SCAN_RECURSIVE : CacheScanner::SCAN_SHALLOW);
				} catch (NotFoundException) {
					// Raced with another delete; the parent job (always present) covers it.
				}
			});
		}
	}

	/**
	 * Run one scan job, classifying failures so no change is silently lost:
	 * contention (file locks, DB lock waits, deadlocks; SQLite reports
	 * "database is locked" as a lock wait) goes to "retry"; anything else to
	 * "failed", which the watcher retries a few times before rescanning the root.
	 *
	 * @param array{scanned: int, skipped: int, ignored: int, retry: list<string>, failed: list<string>, errors: list<string>} $result
	 */
	private function runJob(string $osPath, array &$result, callable $job): void {
		try {
			$job();
			$result['scanned']++;
		} catch (LockedException) {
			$result['retry'][] = $osPath;
		} catch (DbException $e) {
			if (in_array($e->getReason(), [DbException::REASON_LOCK_WAIT_TIMEOUT, DbException::REASON_DEADLOCK], true)) {
				$result['retry'][] = $osPath;
				return;
			}
			$this->fail($osPath, $e, $result);
		} catch (\Throwable $e) {
			$this->fail($osPath, $e, $result);
		}
	}

	private function fail(string $osPath, \Throwable $e, array &$result): void {
		$result['failed'][] = $osPath;
		$result['errors'][] = "$osPath: " . $e->getMessage();
		$this->logger->warning('windows_integration: scan failed', ['path' => $osPath, 'exception' => $e]);
	}

	/**
	 * @param array<string, string> $paths internalPath => osPath
	 * @param list<string> $trees internal paths scanned recursively when they are directories
	 */
	private function applyLocal(string $root, array $paths, array $trees, array &$result, bool $suppress): void {
		$storage = new Local(['datadir' => $root]);
		$plan = self::plan(array_keys($paths), $trees, '',
			fn ($p) => $storage->file_exists($p),
			fn ($p) => $storage->is_dir($p),
			fn ($p, $dir) => $suppress && $this->isCurrent($storage, $p, $dir));
		$result['skipped'] += $plan['skipped'];
		$scanner = $storage->getScanner();
		$propagator = $storage->getPropagator();
		$cache = $storage->getCache();
		foreach ($plan['jobs'] as $path => $recursive) {
			$path = (string)$path;
			$osPath = $root . ($path === '' ? '' : '/' . $path);
			$this->runJob($osPath, $result, function () use ($scanner, $propagator, $cache, $path, $recursive): void {
				$propagator->beginBatch();
				try {
					$scanner->scan($path, $recursive ? CacheScanner::SCAN_RECURSIVE : CacheScanner::SCAN_SHALLOW,
						CacheScanner::REUSE_ETAG | CacheScanner::REUSE_SIZE);
					if ($cache instanceof \OC\Files\Cache\Cache) {
						$cache->correctFolderSize($path);
					}
				} finally {
					$propagator->commitBatch();
				}
				$propagator->propagateChange($path, time());
			});
		}
	}
}
