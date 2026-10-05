"""Collect notices for the Go modules redistributed in a release."""
import json, pathlib, subprocess, sys
out = pathlib.Path(sys.argv[1])
text = subprocess.check_output(['go', 'list', '-deps', '-json', './cmd/dozor'], text=True)
decoder = json.JSONDecoder()
seen = set()
while text.strip():
    package, pos = decoder.raw_decode(text.lstrip())
    text = text.lstrip()[pos:]
    obj = package.get('Module', {})
    if obj.get('Main') or not obj.get('Dir'):
        continue
    if obj['Path'] in seen:
        continue
    seen.add(obj['Path'])
    root = pathlib.Path(obj['Dir'])
    notices = [p for p in root.iterdir() if p.is_file() and p.name.upper().startswith(('LICENSE', 'COPYING', 'NOTICE'))]
    if not notices:
        raise RuntimeError('Missing license notice for ' + obj['Path'])
    for p in notices:
        (out / (obj['Path'].replace('/', '_') + '-' + p.name)).write_bytes(p.read_bytes())
