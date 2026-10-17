<?php

declare(strict_types=1);

/**
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\WindowsIntegration\Command;

use OC\Core\Command\Base;
use OCA\WindowsIntegration\Service\ChangeApplier;
use OCA\WindowsIntegration\Service\RootMap;
use Symfony\Component\Console\Input\InputInterface;
use Symfony\Component\Console\Output\OutputInterface;

/**
 * Long-running request/response loop driven by the ncwin file watcher.
 *
 * Reads one JSON object per line on stdin and writes one JSON object per line
 * on stdout:
 *   {"op":"apply","paths":["D:/ncdata/alice/files/a.txt", ...],"trees":["D:/ncdata/alice/files/newdir", ...]}
 *   {"op":"rescan","root":"D:/ncdata"}
 *   {"op":"roots"}
 *   {"op":"ping"}
 * Every reply carries "ok" and "ms"; apply/rescan add scanned/skipped/ignored/retry/failed/errors.
 * "trees" lists the paths that arrived (added or renamed in); only those
 * directories are scanned recursively.
 * The loop exits on EOF.
 */
class ApplyChanges extends Base {
	public function __construct(
		private ChangeApplier $applier,
		private RootMap $roots,
	) {
		parent::__construct();
	}

	protected function configure(): void {
		parent::configure();
		$this->setName('windows:apply-changes')
			->setDescription('Apply file-system change notifications from the ncwin watcher (JSON lines on stdin)');
	}

	protected function execute(InputInterface $input, OutputInterface $output): int {
		$stdin = fopen('php://stdin', 'rb');
		while (($line = fgets($stdin)) !== false) {
			$line = trim($line);
			if ($line === '') {
				continue;
			}
			$t0 = hrtime(true);
			try {
				$req = json_decode($line, true, 16, JSON_THROW_ON_ERROR);
				$reply = match ($req['op'] ?? '') {
					'apply' => $this->applier->apply(
						array_values(array_filter($req['paths'] ?? [], 'is_string')),
						array_values(array_filter($req['trees'] ?? [], 'is_string')),
					),
					'rescan' => $this->applier->rescan((string)($req['root'] ?? '')),
					'roots' => ['roots' => $this->rootsFresh()],
					'ping' => [],
					default => throw new \InvalidArgumentException('unknown op'),
				};
				$reply['ok'] = true;
			} catch (\Throwable $e) {
				$reply = ['ok' => false, 'error' => $e->getMessage()];
			}
			$reply['ms'] = intdiv(hrtime(true) - $t0, 1_000_000);
			$output->writeln(json_encode($reply, JSON_UNESCAPED_SLASHES | JSON_INVALID_UTF8_SUBSTITUTE));
		}
		return self::SUCCESS;
	}

	private function rootsFresh(): array {
		$this->roots->reset();
		return $this->roots->roots();
	}
}
