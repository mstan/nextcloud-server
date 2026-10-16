<?php

declare(strict_types=1);

/**
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\WindowsIntegration\Command;

use OC\Core\Command\Base;
use OCA\WindowsIntegration\Service\RootMap;
use Symfony\Component\Console\Input\InputInterface;
use Symfony\Component\Console\Output\OutputInterface;

class WatchRoots extends Base {
	public function __construct(
		private RootMap $roots,
	) {
		parent::__construct();
	}

	protected function configure(): void {
		parent::configure();
		$this->setName('windows:watch-roots')
			->setDescription('List the directories the ncwin file watcher must watch');
	}

	protected function execute(InputInterface $input, OutputInterface $output): int {
		$this->writeArrayInOutputFormat($input, $output, $this->roots->roots());
		return self::SUCCESS;
	}
}
