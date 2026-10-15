<?php

declare(strict_types=1);

/**
 * SPDX-FileCopyrightText: 2026 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace Test\Files\Utils;

use OC\Files\Utils\PathHelper;
use PHPUnit\Framework\Attributes\DataProvider;
use Test\TestCase;

class PathHelperOsPathTest extends TestCase {
	public static function absoluteProvider(): array {
		$windows = DIRECTORY_SEPARATOR === '\\';
		return [
			['/srv/data', true],
			['relative/path', false],
			['', false],
			['C:/data', $windows],
			['C:\\data', $windows],
			['d:\\', $windows],
			['\\\\server\\share\\data', $windows],
			['//server/share', $windows],
			['C:data', false],
		];
	}

	#[DataProvider('absoluteProvider')]
	public function testIsAbsoluteOsPath(string $path, bool $expected): void {
		$this->assertSame($expected, PathHelper::isAbsoluteOsPath($path));
	}

	public static function dirnameProvider(): array {
		return [
			['/admin', '/'],
			['/', '/'],
			['/admin/files', '/admin'],
			['/admin/files/a.txt', '/admin/files'],
			['files/a.txt', 'files'],
			['a.txt', '.'],
		];
	}

	/** Same result on every platform; plain dirname() returns '\' for '/x' on Windows. */
	#[DataProvider('dirnameProvider')]
	public function testDirname(string $path, string $expected): void {
		$this->assertSame($expected, PathHelper::dirname($path));
	}

	public function testNormalizeOsPath(): void {
		if (DIRECTORY_SEPARATOR === '\\') {
			$this->assertSame('C:/x/y', PathHelper::normalizeOsPath('C:\\x\\y'));
			$this->assertSame('//srv/share', PathHelper::normalizeOsPath('\\\\srv\\share'));
		} else {
			// '\' is a legal file-name character off Windows: must be untouched.
			$this->assertSame('/srv/a\\b', PathHelper::normalizeOsPath('/srv/a\\b'));
		}
		$this->assertSame('/srv/x', PathHelper::normalizeOsPath('/srv/x'));
	}
}
