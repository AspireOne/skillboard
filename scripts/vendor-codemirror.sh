#!/bin/sh
# Rebuilds web/static/vendor/codemirror.js: one ESM bundle of the CodeMirror pieces
# the editor uses. Run only when bumping versions; the output is committed so the
# app itself needs no Node toolchain.
set -eu

repo=$(cd "$(dirname "$0")/.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

cd "$work"
cat > package.json <<'JSON'
{
  "private": true,
  "dependencies": {
    "@codemirror/commands": "6.11.1",
    "@codemirror/lang-markdown": "6.5.2",
    "@codemirror/language": "6.13.1",
    "@codemirror/state": "6.7.6",
    "@codemirror/view": "6.43.15",
    "@lezer/highlight": "1.2.5",
    "esbuild": "0.28.2"
  }
}
JSON
# esbuild ships its binary through a postinstall script, which pnpm blocks by default.
printf 'allowBuilds:\n  esbuild: true\n' > pnpm-workspace.yaml
cat > entry.js <<'JS'
export { EditorState, Annotation } from '@codemirror/state';
export { EditorView, Decoration, ViewPlugin, WidgetType, keymap, drawSelection, placeholder } from '@codemirror/view';
export { defaultKeymap, history, historyKeymap, indentWithTab } from '@codemirror/commands';
export { syntaxHighlighting, HighlightStyle, syntaxTree } from '@codemirror/language';
export { markdown, markdownLanguage } from '@codemirror/lang-markdown';
export { tags } from '@lezer/highlight';
JS
pnpm install --silent --config.minimum-release-age=0
pnpm exec esbuild entry.js --bundle --format=esm --minify --legal-comments=none \
  --outfile="$repo/web/static/vendor/codemirror.js"
