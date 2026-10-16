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
	 * @return array{scanned: int, ignored: int, retry: list<string>, errors: list<string>}
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

		$result = ['scanned' => 0, 'ignored' => $ignored, 'retry' => [], 'errors' => []];
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
	 * @return array{scanned: int, ignored: int, retry: list<string>, errors: list<string>}
	 */
	public function rescan(string $osRoot): array {
		$osRoot = RootMap::normalize($osRoot);
		$result = ['scanned' => 0, 'ignored' => 0, 'retry' => [], 'errors' => []];
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
			try {
				$scanner->scan($path, $recursive ? CacheScanner::SCAN_RECURSIVE : CacheScanner::SCAN_SHALLOW);
				$result['scanned']++;
			} catch (LockedException) {
				$result['retry'][] = $toOs($path);
			} catch (NotFoundException) {
				// Raced with another delete; the parent job (always present) covers it.
			} catch (\Throwable $e) {
				$result['errors'][] = "$path: " . $e->getMessage();
				$this->logger->warning('windows_integration: scan failed', ['path' => $path, 'exception' => $e]);
			}
		}
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
			try {
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
				$result['scanned']++;
			} catch (LockedException) {
				$result['retry'][] = $root . ($path === '' ? '' : '/' . $path);
			} catch (\Throwable $e) {
				$result['errors'][] = "$root/$path: " . $e->getMessage();
				$this->logger->warning('windows_integration: scan failed', ['root' => $root, 'path' => $path, 'exception' => $e]);
			}
		}
	}
}
