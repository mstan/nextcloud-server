<?php

declare(strict_types=1);

/**
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\WindowsIntegration\Tests;

use OCA\WindowsIntegration\Service\ChangeApplier;
use PHPUnit\Framework\TestCase;

require_once __DIR__ . '/../lib/Service/ChangeApplier.php';

class ChangeApplierPlanTest extends TestCase {
	private const FLOOR = '/alice/files';

	/**
	 * @param list<string> $paths
	 * @param list<string> $trees
	 * @param list<string> $dirs existing directories
	 * @param list<string> $files existing files
	 * @param list<string> $current paths the cache already reflects
	 * @return array{jobs: array<string, bool>, skipped: int}
	 */
	private function plan(array $paths, array $trees, array $dirs, array $files, array $current = []): array {
		$dirs[] = self::FLOOR;
		return ChangeApplier::plan(
			$paths, $trees, self::FLOOR,
			fn (string $p) => in_array($p, $dirs, true) || in_array($p, $files, true),
			fn (string $p) => in_array($p, $dirs, true),
			fn (string $p, bool $isDir) => in_array($p, $current, true),
		);
	}

	public function testFileGetsShallowJobAndParent(): void {
		$r = $this->plan([self::FLOOR . '/d/a.txt'], [], [self::FLOOR . '/d'], [self::FLOOR . '/d/a.txt']);
		$this->assertSame([self::FLOOR . '/d/a.txt' => false, self::FLOOR . '/d' => false], $r['jobs']);
		$this->assertSame(0, $r['skipped']);
	}

	public function testEchoedFileAndParentAreSkipped(): void {
		$d = self::FLOOR . '/d';
		$r = $this->plan([$d . '/a.txt', $d], [], [$d], [$d . '/a.txt'], [$d . '/a.txt', $d, self::FLOOR]);
		$this->assertSame([], $r['jobs']);
		$this->assertSame(3, $r['skipped']);
	}

	public function testChangedFileIsScannedWhileCurrentParentIsSkipped(): void {
		$d = self::FLOOR . '/d';
		$r = $this->plan([$d . '/a.txt'], [], [$d], [$d . '/a.txt'], [$d]);
		$this->assertSame([$d . '/a.txt' => false], $r['jobs']);
		$this->assertSame(1, $r['skipped']);
	}

	public function testDirectoryOutsideTreesIsShallow(): void {
		$d = self::FLOOR . '/d';
		$r = $this->plan([$d], [], [$d], []);
		$this->assertSame([$d => false, self::FLOOR => false], $r['jobs']);
	}

	public function testArrivedDirectoryIsRecursiveAndSubsumesDescendants(): void {
		$d = self::FLOOR . '/new';
		$r = $this->plan([$d, $d . '/x/y.txt'], [$d], [$d, $d . '/x'], [$d . '/x/y.txt']);
		$this->assertSame([$d => true, self::FLOOR => false], $r['jobs']);
	}

	public function testArrivedDirectoryThatIsCurrentIsNotRecursed(): void {
		$d = self::FLOOR . '/moved';
		$r = $this->plan([$d], [$d], [$d], [], [$d, self::FLOOR]);
		$this->assertSame([], $r['jobs']);
		$this->assertSame(2, $r['skipped']);
	}

	public function testTreeFlagOnFileDoesNotMakeItRecursive(): void {
		$f = self::FLOOR . '/f.txt';
		$r = $this->plan([$f], [$f], [], [$f]);
		$this->assertFalse($r['jobs'][$f]);
	}

	public function testRemovedPathFallsBackToNearestExistingAncestor(): void {
		$r = $this->plan([self::FLOOR . '/gone/deeper/x.txt'], [], [], []);
		$this->assertSame([self::FLOOR => false], $r['jobs']);
	}

	public function testRemovedChildScansParentWhenNamesDiffer(): void {
		$d = self::FLOOR . '/d';
		$r = $this->plan([$d . '/gone.txt'], [], [$d], []);
		$this->assertSame([$d => false], $r['jobs']);
	}

	public function testFloorItselfIsPlannedOnce(): void {
		$r = $this->plan([self::FLOOR], [self::FLOOR], [], []);
		$this->assertSame([self::FLOOR => false], $r['jobs']);
	}

	public function testLocalRootFloorIsEmptyString(): void {
		$r = ChangeApplier::plan(['photos/a.jpg'], [], '',
			fn (string $p) => in_array($p, ['', 'photos', 'photos/a.jpg'], true),
			fn (string $p) => in_array($p, ['', 'photos'], true),
			fn (string $p, bool $isDir) => false);
		$this->assertSame(['photos/a.jpg' => false, 'photos' => false], $r['jobs']);
	}

	public function testNumericDirectoryNamesStayStrings(): void {
		$r = ChangeApplier::plan(['2024', '2024/a.jpg'], ['2024'], '',
			fn (string $p) => in_array($p, ['', '2024', '2024/a.jpg'], true),
			fn (string $p) => in_array($p, ['', '2024'], true),
			fn (string $p, bool $isDir) => false);
		$this->assertSame([2024 => true, '' => false], $r['jobs']);
	}
}
