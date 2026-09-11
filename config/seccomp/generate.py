#!/usr/bin/env python3
"""Render OCI profiles from the pinned Moby baseline for cap-drop-ALL Pods."""
import json
from pathlib import Path
base = Path(__file__).resolve().parent
source = json.loads((base / 'upstream/default.json').read_text())
setup = {'clone', 'unshare', 'mount', 'umount2', 'pivot_root', 'chroot', 'setns'}
for arch, audit in [('arm64', 'SCMP_ARCH_AARCH64'), ('amd64', 'SCMP_ARCH_X86_64')]:
    rules = []
    for original in source['syscalls']:
        rule = dict(original)
        inc, exc = rule.pop('includes', {}), rule.pop('excludes', {})
        if inc.get('caps') or (inc.get('arches') and arch not in inc['arches']):
            continue
        if arch in exc.get('arches', []):
            continue
        # minKernel applies on the supported >=6.3 deployment baseline.
        rule['names'] = [n for n in rule['names'] if n not in setup]
        if rule['names']:
            rules.append(rule)
    rules.append({'names': sorted(setup), 'action': 'SCMP_ACT_ALLOW'})
    result = {'defaultAction': source['defaultAction'], 'defaultErrnoRet': source['defaultErrnoRet'],
              'architectures': [audit], 'syscalls': rules}
    (base / f'bwrap-setup-{arch}.json').write_text(json.dumps(result, indent=2) + '\n')
