#!/usr/bin/env python3
"""Fail on PHPUnit failures/errors except those known to fail upstream too.

usage: junit_check.py <junit dir> [known-failures file]

Known failures are "Class::method" lines (data-set suffixes ignored). Each one
must also fail on stock upstream Linux (windows/tests/linux-compare.sh), so
the list only ever records upstream bugs, never Windows ones.
"""
import glob
import os
import re
import sys
import xml.etree.ElementTree as ET

junit_dir = sys.argv[1]
known_file = sys.argv[2] if len(sys.argv) > 2 else os.path.join(os.path.dirname(__file__), 'phpunit-known-upstream.txt')
known = set()
with open(known_file, encoding='utf-8') as f:
    for line in f:
        line = line.strip()
        if line and not line.startswith('#'):
            known.add(line)

unexpected, tolerated = [], []
for path in sorted(glob.glob(os.path.join(junit_dir, '*.xml'))):
    for tc in ET.parse(path).iter('testcase'):
        bad = tc.findall('failure') + tc.findall('error')
        if not bad:
            continue
        name = re.sub(r' with data set .*$', '', tc.get('name', ''))
        cls = tc.get('classname', '').replace('.', chr(92))  # JUnit uses '.', PHP namespaces '\'
        key = f'{cls}::{name}'
        first = (bad[0].text or '').strip().splitlines()
        msg = first[1] if len(first) > 1 else (first[0] if first else '')
        (tolerated if key in known else unexpected).append(f'{key}: {msg[:200]}')

for t in tolerated:
    print('known upstream:', t)
for u in unexpected:
    print('UNEXPECTED:', u)
print(f'{len(unexpected)} unexpected, {len(tolerated)} known upstream failure(s)')
sys.exit(1 if unexpected else 0)
