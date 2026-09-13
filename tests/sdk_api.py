"""Check plugin API names against an installed Adobe Lightroom SDK reference.

This checks imports, namespace members and method names, not argument types or
callback timing. Behavioral tests and an actual Lightroom publish remain needed.
"""

import re
import sys
from pathlib import Path

if len(sys.argv) != 2 or not sys.argv[1]:
    sys.exit('Usage: python3 tests/sdk_api.py /path/to/Lightroom/sdk')

reference = Path(sys.argv[1]).expanduser() / 'API Reference' / 'modules'
if not reference.is_dir():
    sys.exit(f'Cannot find SDK API Reference: {reference}')

anchors = {
    path.stem: set(re.findall(r'<a name="([^"]+)"', path.read_text()))
    for path in reference.glob('*.html')
}
methods = {
    anchor.split(':', 1)[1]
    for names in anchors.values() for anchor in names if ':' in anchor
}
# SDK Guide, "Using built-in Lua features", pp. 19-20: io, math and
# string are available; os/table/coroutine/debug have restricted members.
lua_methods = {
    'close', 'flush', 'lines', 'read', 'seek', 'setvbuf', 'write',
    'byte', 'char', 'dump', 'find', 'format', 'gmatch', 'gsub', 'len',
    'lower', 'match', 'rep', 'reverse', 'sub', 'upper',
}
restricted = {
    'os': {'clock', 'date', 'time', 'tmpname'},
    'coroutine': {'canYield', 'running'},
    'debug': {'getInfo'},
    'package': set(),
}
errors = []
count = 0
for path in sorted(Path('R2Publisher.lrplugin').glob('*.lua')):
    source = path.read_text()
    # Preserve quoted strings while stripping comments (including long ones).
    source = re.sub(
        r"'(?:\\.|[^'\\])*'|\"(?:\\.|[^\"\\])*\"|--\[\[.*?\]\]|--[^\n]*",
        lambda m: '' if m[0].startswith('--') else m[0], source, flags=re.S,
    )
    for alias, namespace in re.findall(r"local\s+(\w+)\s*=\s*import\s*['\"](\w+)['\"]", source):
        if namespace not in anchors:
            errors.append(f'{path}: undocumented import {namespace}')
        for member in set(re.findall(r'\b' + alias + r'\.(\w+)', source)):
            count += 1
            if namespace + '.' + member not in anchors.get(namespace, set()):
                errors.append(f'{path}: undocumented API {namespace}.{member}')
    for method in set(re.findall(r':(\w+)\s*[({]', source)):
        count += 1
        if method not in methods | lua_methods:
            errors.append(f'{path}: undocumented method {method}')
    for namespace, allowed in restricted.items():
        for member in set(re.findall(r'\b' + namespace + r'\.(\w+)', source)):
            if member not in allowed:
                errors.append(f'{path}: unavailable Lua API {namespace}.{member}')
    for member in re.findall(r'\btable\.(getn|setn|maxn)\b', source):
        errors.append(f'{path}: unavailable Lua API table.{member}')

if errors:
    sys.exit('\n'.join(errors))
print(f'Lightroom SDK API name check passed ({count} references)')
