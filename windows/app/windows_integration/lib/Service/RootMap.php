<?php

declare(strict_types=1);

/**
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\WindowsIntegration\Service;

use OCP\App\IAppManager;
use OCP\IConfig;
use OCP\Server;

/**
 * Maps absolute OS paths to the Nextcloud storage that owns them.
 *
 * Two kinds of root are watched:
 *  - "home": <datadirectory>/<uid>/files, scanned through the user's view
 *  - "local": the datadir of a files_external "Local" mount, scanned directly
 */
class RootMap {
	/** @var list<array{kind: string, path: string, user?: string}>|null */
	private ?array $roots = null;

	public function __construct(
		private IConfig $config,
		private IAppManager $appManager,
	) {
	}

	public static function normalize(string $path): string {
		$path = str_replace('\\', '/', $path);
		return rtrim($path, '/');
	}

	public function dataDir(): string {
		return self::normalize($this->config->getSystemValueString('datadirectory', \OC::$SERVERROOT . '/data'));
	}

	/**
	 * @return list<array{kind: string, path: string}>
	 */
	public function roots(): array {
		if ($this->roots !== null) {
			return $this->roots;
		}
		$roots = [['kind' => 'data', 'path' => $this->dataDir()]];
		if ($this->appManager->isEnabledForAnyone('files_external')) {
			/** @var \OCA\Files_External\Service\GlobalStoragesService $service */
			$service = Server::get(\OCA\Files_External\Service\GlobalStoragesService::class);
			foreach ($service->getAllStorages() as $storage) {
				if ($storage->getBackend()->getIdentifier() !== 'local') {
					continue;
				}
				$dir = $storage->getBackendOption('datadir');
				if (is_string($dir) && $dir !== '') {
					$roots[] = ['kind' => 'local', 'path' => self::normalize($dir)];
				}
			}
		}
		// Longest first, so a mount inside the data directory wins over it.
		usort($roots, fn ($a, $b) => strlen($b['path']) <=> strlen($a['path']));
		return $this->roots = $roots;
	}

	public function reset(): void {
		$this->roots = null;
	}

	/**
	 * Resolve an absolute OS path. For "home", path is the Nextcloud path
	 * (/<uid>/files/...); for "local", it is the storage-internal path.
	 *
	 * @return array{kind: 'home', user: string, path: string}|array{kind: 'local', root: string, path: string}|null
	 */
	public function resolve(string $osPath): ?array {
		$osPath = self::normalize($osPath);
		foreach ($this->roots() as $root) {
			if (!self::under($osPath, $root['path'])) {
				continue;
			}
			$rel = ltrim(substr($osPath, strlen($root['path'])), '/');
			if ($root['kind'] === 'local') {
				return ['kind' => 'local', 'root' => $root['path'], 'path' => $rel];
			}
			// data dir: only <uid>/files[/...] is user-visible content.
			$parts = explode('/', $rel, 3);
			if (count($parts) < 2 || $parts[1] !== 'files' || $parts[0] === '' || str_starts_with($parts[0], 'appdata_')) {
				return null;
			}
			return ['kind' => 'home', 'user' => $parts[0], 'path' => '/' . $parts[0] . '/files' . (isset($parts[2]) && $parts[2] !== '' ? '/' . $parts[2] : '')];
		}
		return null;
	}

	/**
	 * Case-insensitive prefix test: NTFS paths compare case-insensitively.
	 */
	public static function under(string $path, string $root): bool {
		$lr = strlen($root);
		return strncasecmp($path, $root, $lr) === 0 && (strlen($path) === $lr || $path[$lr] === '/');
	}
}
