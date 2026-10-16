<?php

declare(strict_types=1);

/**
 * SPDX-FileCopyrightText: 2026 Nextcloud GmbH and Nextcloud contributors
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace Test\Files\Storage;

use OC\Files\Storage\Local;
use OCP\Files\ForbiddenException;
use OCP\IConfig;
use OCP\ITempManager;
use OCP\Server;
use Test\TestCase;

/**
 * Creating an item whose name differs only in letter case from an existing
 * sibling must fail on a case-insensitive volume instead of silently
 * overwriting it. Skipped where the temp volume is case-sensitive.
 */
#[\PHPUnit\Framework\Attributes\Group('DB')]
class LocalCaseInsensitiveTest extends TestCase {
	private string $tmpDir;
	private Local $storage;

	protected function setUp(): void {
		parent::setUp();
		$this->tmpDir = Server::get(ITempManager::class)->getTemporaryFolder();
		file_put_contents($this->tmpDir . '/Probe', '');
		if (!file_exists($this->tmpDir . '/probe')) {
			$this->markTestSkipped('temporary directory is on a case-sensitive volume');
		}
		unlink($this->tmpDir . '/Probe');
		$config = Server::get(IConfig::class);
		$config->setSystemValue('localstorage.case_insensitive', true);
		$this->storage = new Local(['datadir' => $this->tmpDir]);
	}

	protected function tearDown(): void {
		Server::get(IConfig::class)->deleteSystemValue('localstorage.case_insensitive');
		parent::tearDown();
	}

	public function testFopenWriteRejectsCaseVariant(): void {
		$this->storage->file_put_contents('Report.txt', 'original');
		$this->expectException(ForbiddenException::class);
		try {
			$this->storage->fopen('report.txt', 'w');
		} finally {
			$this->assertSame('original', file_get_contents($this->tmpDir . '/Report.txt'));
		}
	}

	public function testFilePutContentsRejectsCaseVariant(): void {
		$this->storage->file_put_contents('a.txt', 'x');
		$this->expectException(ForbiddenException::class);
		$this->storage->file_put_contents('A.TXT', 'y');
	}

	public function testMkdirRejectsCaseVariant(): void {
		$this->storage->mkdir('Docs');
		$this->expectException(ForbiddenException::class);
		$this->storage->mkdir('docs');
	}

	public function testRenameOntoCaseVariantRejected(): void {
		$this->storage->file_put_contents('one.txt', '1');
		$this->storage->file_put_contents('Two.txt', '2');
		$this->expectException(ForbiddenException::class);
		$this->storage->rename('one.txt', 'two.txt');
	}

	public function testCaseOnlyRenameAllowed(): void {
		$this->storage->file_put_contents('photo.jpg', 'p');
		$this->assertTrue($this->storage->rename('photo.jpg', 'Photo.jpg'));
		$this->assertTrue($this->storage->file_exists('Photo.jpg'));
		$this->assertFalse($this->storage->file_exists('photo.jpg'));
	}

	/** On Windows '\' is an OS separator: a name containing it must never reach the disk. */
	public function testBackslashNameRefused(): void {
		if (DIRECTORY_SEPARATOR !== '\\') {
			$this->markTestSkipped('only meaningful where \\ is a path separator');
		}
		$this->storage->mkdir('a');
		$this->expectException(ForbiddenException::class);
		try {
			$this->storage->file_put_contents('a\\b.txt', 'x');
		} finally {
			$this->assertFileDoesNotExist($this->tmpDir . '/a/b.txt');
		}
	}

	public function testExactNameStillWritable(): void {
		$this->storage->file_put_contents('same.txt', '1');
		$this->assertSame(1, $this->storage->file_put_contents('same.txt', '2'));
		$this->assertSame('2', $this->storage->file_get_contents('same.txt'));
	}
}
