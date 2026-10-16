<?php

declare(strict_types=1);

/**
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\WindowsIntegration\Service;

use OC\Files\Cache\Scanner as CacheScanner;
use OC\Files\SetupManager;
use OC\Files\Storage\Local;
use OC\Files\Utils\Scanner;
use OCP\DB\Exception as DbException;
use OCP\EventDispatcher\IEventDispatcher;
use OCP\Files\NotFoundException;
use OCP\IDBConnection;
use OCP\IUserManager;
use OCP\Lock\LockedException;
use Psr\Log\LoggerInterface;

/**
 * Applies a batch of file-system change notifications to the file cache.
 *
 * For every changed path P:
 *  - if P exists: scan P (recursively when it is a directory, since a whole
 *    tree may have been copied or moved in);
 *  - scan P's parent shallowly, which picks up deletions and the old name of
 *    a rename.
 * A path whose ancestors no longer exist falls back to the nearest existing one.
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
	 * @return array{scanned: int, ignored: int, retry: list<string>, failed: list<string>, errors: list<string>}
	 */
	public function apply(array $osPaths): array {
		$home = [];  // uid => [ncPath => osPath]
		$local = []; // root => [internalPath => osPath]
		$ignored = 0;
		foreach (array_unique($osPaths) as $os) {
			$r = $this->roots->resolve($os);
			if ($r === null) {
				$ignored++;
			} elseif ($r['kind'] === 'home') {
				$home[$r['user']][$r['path']] = $os;
			} else {
				$local[$r['root']][$r['path']] = $os;
			}
		}

		$result = ['scanned' => 0, 'ignored' => $ignored, 'retry' => [], 'failed' => [], 'errors' => []];
		foreach ($home as $uid => $paths) {
			$this->applyHome((string)$uid, $paths, $result);
		}
		foreach ($local as $root => $paths) {
			$this->applyLocal((string)$root, $paths, $result);
		}
		return $result;
	}

	/**
	 * Full rescan of one watched root (after a watcher buffer overflow, or nightly).
	 *
	 * @return array{scanned: int, ignored: int, retry: list<string>, failed: list<string>, errors: list<string>}
	 */
	public function rescan(string $osRoot): array {
		$osRoot = RootMap::normalize($osRoot);
		$result = ['scanned' => 0, 'ignored' => 0, 'retry' => [], 'failed' => [], 'errors' => []];
		if ($osRoot === $this->roots->dataDir()) {
			foreach ($this->userManager->getSeenUsers() as $user) {
				$this->applyHome($user->getUID(), ['/' . $user->getUID() . '/files' => $osRoot . '/' . $user->getUID() . '/files'], $result);
			}
			return $result;
		}
		$r = $this->roots->resolve($osRoot);
		if ($r === null) {
			$result['errors'][] = "not a watched root: $osRoot";
		} elseif ($r['kind'] === 'home') {
			$this->applyHome($r['user'], [$r['path'] => $osRoot], $result);
		} else {
			$this->applyLocal($r['root'], [$r['path'] => $osRoot], $result);
		}
		return $result;
	}

	/**
	 * Expand changed paths into scan jobs: [path => recursive?]. Parents are
	 * scanned shallowly; a recursive scan of an ancestor subsumes descendants.
	 *
	 * @param list<string> $paths
	 * @param callable(string): bool $isDir
	 * @param callable(string): bool $exists
	 * @return array<string, bool>
	 */
	public static function plan(array $paths, string $floor, callable $exists, callable $isDir): array {
		$jobs = [];
		foreach ($paths as $p) {
			if ($p !== $floor && $exists($p)) {
				$jobs[$p] = ($jobs[$p] ?? false) || $isDir($p);
			}
			$parent = self::parentOf($p, $floor);
			while ($parent !== $floor && !$exists($parent)) {
				$parent = self::parentOf($parent, $floor);
			}
			$jobs[$parent] = $jobs[$parent] ?? false;
		}
		// Drop jobs covered by a recursive ancestor job.
		$recursive = array_keys(array_filter($jobs));
		foreach (array_keys($jobs) as $p) {
			foreach ($recursive as $r) {
				if ($p !== $r && ($r === '' || str_starts_with($p . '/', rtrim($r, '/') . '/'))) {
					unset($jobs[$p]);
					break;
				}
			}
		}
		return $jobs;
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
	 */
	private function applyHome(string $uid, array $paths, array &$result): void {
		$user = $this->userManager->get($uid);
		if ($user === null) {
			$result['ignored'] += count($paths);
			return;
		}
		$floor = '/' . $uid . '/files';
		$toOs = fn (string $nc) => $this->roots->dataDir() . '/' . $uid . '/files' . substr($nc, strlen($floor));
		$jobs = self::plan(array_keys($paths), $floor,
			fn ($p) => file_exists($toOs($p)),
			fn ($p) => is_dir($toOs($p)));

		$scanner = new Scanner($user, $this->db, $this->eventDispatcher, $this->logger, $this->setupManager);
		foreach ($jobs as $path => $recursive) {
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
	 * @param array{scanned: int, ignored: int, retry: list<string>, failed: list<string>, errors: list<string>} $result
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
	 */
	private function applyLocal(string $root, array $paths, array &$result): void {
		$storage = new Local(['datadir' => $root]);
		$jobs = self::plan(array_keys($paths), '',
			fn ($p) => $storage->file_exists($p),
			fn ($p) => $storage->is_dir($p));
		$scanner = $storage->getScanner();
		$propagator = $storage->getPropagator();
		$cache = $storage->getCache();
		foreach ($jobs as $path => $recursive) {
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
