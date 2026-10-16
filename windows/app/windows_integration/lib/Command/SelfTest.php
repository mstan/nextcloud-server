<?php

declare(strict_types=1);

/**
 * SPDX-License-Identifier: AGPL-3.0-or-later
 */

namespace OCA\WindowsIntegration\Command;

use OC\Core\Command\Base;
use OCA\Files\Service\SettingsService;
use OCA\WindowsIntegration\Service\RootMap;
use OCP\IAppConfig;
use OCP\IConfig;
use OCP\Server;
use Symfony\Component\Console\Input\InputInterface;
use Symfony\Component\Console\Output\OutputInterface;

/**
 * Functional checks of the Windows platform assumptions ncwin relies on.
 * Each check exercises the real behaviour (e.g. actually creates a long path)
 * rather than reading a setting that is supposed to imply it.
 */
class SelfTest extends Base {
	public function __construct(
		private IConfig $config,
		private IAppConfig $appConfig,
		private RootMap $roots,
	) {
		parent::__construct();
	}

	protected function configure(): void {
		parent::configure();
		$this->setName('windows:selftest')
			->setDescription('Check that this Windows host behaves the way ncwin requires');
	}

	protected function execute(InputInterface $input, OutputInterface $output): int {
		$results = [];
		$check = function (string $name, callable $fn) use (&$results): void {
			try {
				$r = $fn();
				$results[] = ['check' => $name, 'ok' => $r === true, 'detail' => $r === true ? '' : (string)$r];
			} catch (\Throwable $e) {
				$results[] = ['check' => $name, 'ok' => false, 'detail' => get_class($e) . ': ' . $e->getMessage()];
			}
		};

		$check('platform is Windows', fn () => PHP_OS_FAMILY === 'Windows' ?: PHP_OS_FAMILY);
		$check('PHP extensions', function () {
			$missing = array_filter(['apcu', 'ctype', 'curl', 'dom', 'fileinfo', 'gd', 'intl', 'mbstring', 'openssl', 'sodium', 'zip', 'zlib', 'pdo', 'gmp', 'bcmath', 'exif'],
				fn ($e) => !extension_loaded($e));
			return $missing === [] ?: 'missing: ' . implode(', ', $missing);
		});
		$check('APCu local cache configured', fn () => $this->config->getSystemValueString('memcache.local') === '\OC\Memcache\APCu' ?: 'memcache.local = ' . $this->config->getSystemValueString('memcache.local'));
		$check('background jobs in cron mode', fn () => $this->appConfig->getValueString('core', 'backgroundjobs_mode', 'ajax') === 'cron' ?: 'mode is ' . $this->appConfig->getValueString('core', 'backgroundjobs_mode', 'ajax'));
		$check('localstorage.case_insensitive', fn () => $this->config->getSystemValueBool('localstorage.case_insensitive') ?: 'not enabled');
		$check('Windows-compatible filename rules', fn () => Server::get(SettingsService::class)->hasFilesWindowsSupport() ?: 'run: occ files:windows-compatible-filenames --enable');

		$dir = $this->roots->dataDir() . '/.ncwin-selftest-' . bin2hex(random_bytes(4));
		$check('data directory writable', fn () => mkdir($dir) ?: "cannot create $dir");
		$check('paths longer than 260 characters', function () use ($dir) {
			$long = $dir . '/' . str_repeat('d', 120) . '/' . str_repeat('e', 120);
			if (!mkdir($long, 0777, true)) {
				return 'mkdir failed; enable LongPathsEnabled';
			}
			$f = $long . '/' . str_repeat('f', 60) . '.txt';
			if (file_put_contents($f, 'x') !== 1 || file_get_contents($f) !== 'x') {
				return 'write/read failed at ' . strlen($f) . ' chars';
			}
			unlink($f);
			rmdir($long);
			rmdir(dirname($long));
			return true;
		});
		$check('case-insensitive volume detected', function () use ($dir) {
			file_put_contents("$dir/Case.txt", 'x');
			$r = file_exists("$dir/case.txt");
			unlink("$dir/Case.txt");
			return $r ?: 'volume is case-sensitive; localstorage.case_insensitive should be disabled for it';
		});
		$check('rename onto a file open for reading', function () use ($dir) {
			file_put_contents("$dir/target", 'old');
			file_put_contents("$dir/source", 'new');
			$h = fopen("$dir/target", 'rb');
			$ok = @rename("$dir/source", "$dir/target");
			fclose($h);
			$content = @file_get_contents("$dir/target");
			@unlink("$dir/source");
			@unlink("$dir/target");
			return ($ok && $content === 'new') ?: 'rename failed while target was open (sharing violation); retry logic required';
		});
		$check('unlink a file open for reading', function () use ($dir) {
			file_put_contents("$dir/open", 'x');
			$h = fopen("$dir/open", 'rb');
			$ok = @unlink("$dir/open");
			fclose($h);
			@unlink("$dir/open");
			return $ok ?: 'unlink failed while file was open (sharing violation); retry logic required';
		});
		@rmdir($dir);

		$failed = array_filter($results, fn ($r) => !$r['ok']);
		if ($input->getOption('output') === self::OUTPUT_FORMAT_PLAIN) {
			foreach ($results as $r) {
				$output->writeln(sprintf('%s %s%s', $r['ok'] ? '<info>PASS</info>' : '<error>FAIL</error>', $r['check'], $r['detail'] !== '' ? ' - ' . $r['detail'] : ''));
			}
		} else {
			$this->writeArrayInOutputFormat($input, $output, $results);
		}
		return $failed === [] ? self::SUCCESS : self::FAILURE;
	}
}
