#!/usr/bin/env python3
"""Flag Windows-hostile PHP patterns that are not explicitly reviewed.

usage: lint_windows.py [--update] [<git ref>]

Scans lib/, core/, apps/ and the root entry points (at <git ref> via
`git grep`, or the work tree) for:

  dirname   bare dirname() calls. On Windows dirname('/x') returns '\\', so
  basename  and basename('a\\b') returns 'b'; virtual paths must use
            PathHelper::dirname()/basename(). Bare calls are allowed only
            on OS paths, listed in lint-windows-allow.txt.
  posix     posix_*() calls not guarded by function_exists() on the same or
            the preceding three lines (PHP for Windows has no ext-posix).

Allowlist entries are "<kind>\\t<path>\\t<stripped source line>", so they
survive line-number drift but not code changes. --update rewrites the
allowlist from the current findings (review the diff before committing).
Exit status 1 when unreviewed findings exist.
"""
import os
import re
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ALLOW = os.path.join(HERE, 'lint-windows-allow.txt')
ROOT = os.path.normpath(os.path.join(HERE, '..', '..'))
SKIP = re.compile(r'(^|/)(tests?|vendor|composer|3rdparty|node_modules)/')
BARE_DIRNAME = re.compile(r'(?<![\w\\>:$])(?<!function )dirname\(')
BARE_BASENAME = re.compile(r'(?<![\w\\>:$])(?<!function )basename\(')
POSIX = re.compile(r'(?<![\w>:$])posix_\w+\(')
GUARD = re.compile(r"function_exists\(\s*'posix_|PHP_OS_FAMILY|DIRECTORY_SEPARATOR")


def grep(ref, pattern):
    cmd = ['git', '-C', ROOT, 'grep', '-n', '-I', '-E', pattern]
    if ref:
        cmd.append(ref)
    cmd += ['--', 'lib', 'core', 'apps', 'occ', 'console.php', 'cron.php']
    out = subprocess.run(cmd, capture_output=True, text=True, encoding='utf-8').stdout
    for line in out.splitlines():
        if ref:
            line = line.split(':', 1)[1]  # drop "<ref>:"
        path, num, text = line.split(':', 2)
        if path.endswith('.php') or path in ('occ', 'console.php', 'cron.php'):
            yield path, int(num), text


def show(ref, path):
    if ref:
        return subprocess.run(['git', '-C', ROOT, 'show', f'{ref}:{path}'], capture_output=True, text=True, encoding='utf-8').stdout.splitlines()
    with open(os.path.join(ROOT, path), encoding='utf-8') as f:
        return f.read().splitlines()


def findings(ref):
    out = set()
    for kind, pattern in (('dirname', BARE_DIRNAME), ('basename', BARE_BASENAME)):
        for path, _, text in grep(ref, kind + r'\('):
            if SKIP.search(path) or text.lstrip().startswith(('*', '//', '#')):
                continue
            if pattern.search(text) and '__DIR__' not in text and '__FILE__' not in text:
                out.add((kind, path, text.strip()))
    cache = {}
    for path, num, text in grep(ref, r'posix_[a-z_]+\('):
        if SKIP.search(path) or text.lstrip().startswith(('*', '//', '#')) or not POSIX.search(text):
            continue
        lines = cache.setdefault(path, show(ref, path))
        context = lines[max(0, num - 4):num]
        if not any(GUARD.search(l) for l in context):
            out.add(('posix', path, text.strip()))
    return out


def main():
    args = [a for a in sys.argv[1:] if a != '--update']
    ref = args[0] if args else None
    found = findings(ref)
    if '--update' in sys.argv:
        with open(ALLOW, 'w', encoding='utf-8', newline='\n') as f:
            f.write('# Reviewed Windows-lint exceptions (see lint_windows.py). kind<TAB>path<TAB>line\n')
            for k, p, t in sorted(found):
                f.write(f'{k}\t{p}\t{t}\n')
        print(f'wrote {len(found)} entries to {ALLOW}')
        return 0
    allowed = set()
    with open(ALLOW, encoding='utf-8') as f:
        for line in f:
            if line.startswith('#') or not line.strip():
                continue
            k, p, t = line.rstrip('\n').split('\t', 2)
            allowed.add((k, p, t))
    new = sorted(found - allowed)
    for k, p, t in new:
        print(f'{k}: {p}: {t}')
    if new:
        print(f'\n{len(new)} unreviewed Windows-hostile site(s). Route virtual paths through '
              'PathHelper::dirname() / guard posix_*(), or add OS-path sites to lint-windows-allow.txt.')
        return 1
    print(f'ok: {len(found)} reviewed site(s), nothing new')
    return 0


if __name__ == '__main__':
    sys.exit(main())
