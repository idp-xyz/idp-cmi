#!/usr/bin/env python3
"""Regenerate the self-contained font subset after editing the Markdown/build script.
Requires fonttools; the legally reusable original font is included in assets/.
"""
from pathlib import Path
from fontTools import subset
from fontTools.ttLib import TTFont
root = Path(__file__).resolve().parent
text = (root / 'CMI_Master.md').read_text(encoding='utf-8') + (root / 'build.mjs').read_text(encoding='utf-8')
font = TTFont(root / 'assets' / 'DroidSansFallback.ttf')
options = subset.Options()
options.hinting = False
options.recalc_timestamp = False
s = subset.Subsetter(options=options)
s.populate(text=text)
s.subset(font)
font.flavor = 'woff'
font.save(root / 'assets' / 'DroidSansFallback-subset.woff')
print('Updated font subset for current source and interface.')
