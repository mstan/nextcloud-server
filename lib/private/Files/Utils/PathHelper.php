<?php

declare(strict_types=1);
/**
 * SPDX-FileCopyrightText: 2022 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OC\Files\Utils;

class PathHelper {
	/**
	 * Make a path relative to a root path, or return null if the path is outside the root
	 *
	 * @param string $root
	 * @param string $path
	 * @return ?string
	 */
	public static function getRelativePath(string $root, string $path) {
		if ($root === '' || $root === '/') {
			return self::normalizePath($path);
		}
		if ($path === $root) {
			return '/';
		} elseif (!str_starts_with($path, $root . '/')) {
			return null;
		} else {
			$path = substr($path, strlen($root));
			return self::normalizePath($path);
		}
	}

	/**
	 * @param string $path
	 * @return string
	 */
	public static function normalizePath(string $path): string {
		if ($path === '' || $path === '/') {
			return '/';
		}
		// No null bytes
		$path = str_replace(chr(0), '', $path);
		//no windows style slashes
		$path = str_replace('\\', '/', $path);
		//add leading slash
		if ($path[0] !== '/') {
			$path = '/' . $path;
		}
		//remove duplicate slashes
		while (str_contains($path, '//')) {
			$path = str_replace('//', '/', $path);
		}
		//remove trailing slash
		$path = rtrim($path, '/');

		return $path;
	}

	/**
	 * dirname() for Nextcloud's '/'-separated virtual and storage paths.
	 *
	 * Identical to dirname() except on Windows, where dirname('/x') and
	 * dirname('/') return '\' instead of '/'. Code walking up a path until it
	 * reaches '/' would otherwise never terminate.
	 */
	public static function dirname(string $path, int $levels = 1): string {
		$dir = dirname($path, $levels);
		return ($dir === '\\' && DIRECTORY_SEPARATOR === '\\') ? '/' : $dir;
	}

	/**
	 * basename() for Nextcloud's '/'-separated virtual and storage paths.
	 *
	 * On Windows basename() also splits on '\', so basename('a\b') is 'b' and
	 * name validation never sees the forbidden '\'. This splits on '/' only,
	 * everywhere.
	 */
	public static function basename(string $path, string $suffix = ''): string {
		if (DIRECTORY_SEPARATOR !== '\\') {
			return basename($path, $suffix);
		}
		$path = rtrim($path, '/');
		$i = strrpos($path, '/');
		$base = $i === false ? $path : substr($path, $i + 1);
		if ($suffix !== '' && $suffix !== $base && str_ends_with($base, $suffix)) {
			$base = substr($base, 0, -strlen($suffix));
		}
		return $base;
	}

	/**
	 * Use '/' as the separator in an OS file-system path.
	 *
	 * On Windows, realpath(), __DIR__, getcwd() and directory iterators return
	 * '\'-separated paths, while Nextcloud compares OS paths with '/'. Windows
	 * accepts '/' everywhere. A no-op on other platforms, where '\' is a valid
	 * file-name character.
	 */
	public static function normalizeOsPath(string $path): string {
		return DIRECTORY_SEPARATOR === '\\' ? str_replace('\\', '/', $path) : $path;
	}

	/**
	 * Whether an OS file-system path is absolute: '/...' everywhere, plus
	 * drive ('C:/...', 'C:\...') and UNC ('\\server\share') paths on Windows.
	 */
	public static function isAbsoluteOsPath(string $path): bool {
		if ($path !== '' && $path[0] === '/') {
			return true;
		}
		return DIRECTORY_SEPARATOR === '\\' && preg_match('#^([a-zA-Z]:[\\\\/]|[\\\\/]{2}[^\\\\/])#', $path) === 1;
	}
}
