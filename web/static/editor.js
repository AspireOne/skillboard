// Markdown editor with Obsidian-style live preview: Markdown renders in place, and a
// line shows its raw syntax only while the cursor is on it.
import {
  Annotation,
  Decoration,
  EditorState,
  EditorView,
  HighlightStyle,
  ViewPlugin,
  WidgetType,
  defaultKeymap,
  drawSelection,
  history,
  historyKeymap,
  indentWithTab,
  keymap,
  markdown,
  markdownLanguage,
  placeholder,
  syntaxHighlighting,
  syntaxTree,
  tags,
} from './vendor/codemirror.js';

const FRONTMATTER = /^---\n[\s\S]*?\n---(?:\n|$)/;
// Inline syntax hidden off the cursor line.
const HIDDEN_MARKS = new Set(['EmphasisMark', 'StrikethroughMark', 'QuoteMark']);

// Marks content replaced from disk, so it is not reported as a user edit.
const fromDisk = Annotation.define();

class BulletWidget extends WidgetType {
  eq() { return true; }
  toDOM() { return Object.assign(document.createElement('span'), { className: 'cm-bullet', textContent: '•' }); }
}

// Shows an element the caller owns and keeps up to date, so its content can change
// without redecorating the editor.
class SlotWidget extends WidgetType {
  constructor(el) { super(); this.el = el; }
  eq(other) { return other.el === this.el; }
  toDOM() { return this.el; }
  ignoreEvent() { return true; }
}

class RuleWidget extends WidgetType {
  eq() { return true; }
  toDOM() { return Object.assign(document.createElement('span'), { className: 'cm-rule' }); }
}

const hide = Decoration.replace({});
const bullet = Decoration.replace({ widget: new BulletWidget() });
const rule = Decoration.replace({ widget: new RuleWidget() });
const line = (cls) => Decoration.line({ class: cls });

function frontmatterEnd(doc) {
  const head = doc.sliceString(0, Math.min(doc.length, 8000));
  return head.match(FRONTMATTER)?.[0].length ?? 0;
}

// Lines touched by any selection range, but only while the editor has focus:
// an unfocused editor reads as a clean rendered page.
function activeLines(view) {
  const lines = new Set();
  if (!view.hasFocus) return lines;
  for (const r of view.state.selection.ranges) {
    const last = view.state.doc.lineAt(r.to).number;
    for (let n = view.state.doc.lineAt(r.from).number; n <= last; n++) lines.add(n);
  }
  return lines;
}

function buildDecorations(view, frontmatterNote) {
  const { doc } = view.state;
  const fmEnd = frontmatterEnd(doc);
  const active = activeLines(view);
  const isActive = (pos) => active.has(doc.lineAt(pos).number);
  const decos = [];

  // Frontmatter would otherwise parse as a horizontal rule plus a setext heading.
  if (fmEnd > 0) {
    const lastLine = doc.lineAt(fmEnd - 1).number;
    for (let n = 1; n <= lastLine; n++) {
      const edge = n === 1 ? ' cm-fm-first' : n === lastLine ? ' cm-fm-last' : '';
      decos.push(line('cm-fm' + edge).range(doc.line(n).from));
    }
    if (frontmatterNote) {
      const end = doc.line(lastLine).to;
      decos.push(Decoration.widget({ widget: new SlotWidget(frontmatterNote), side: 1 }).range(end));
    }
  }

  for (const { from, to } of view.visibleRanges) {
    syntaxTree(view.state).iterate({
      from: Math.max(from, fmEnd),
      to,
      enter(node) {
        if (node.from < fmEnd) return;
        const name = node.name;

        const heading = /^(?:ATX|Setext)Heading(\d)$/.exec(name);
        if (heading) {
          decos.push(line(`cm-h cm-h${heading[1]}`).range(doc.lineAt(node.from).from));
          return;
        }

        if (name === 'HeaderMark') {
          if (isActive(node.from)) return;
          const l = doc.lineAt(node.from);
          // Hide "## " including its trailing space; a setext underline hides whole.
          const end = node.to < l.to && doc.sliceString(node.to, node.to + 1) === ' ' ? node.to + 1 : node.to;
          decos.push(hide.range(node.from, end));
          return;
        }

        if (HIDDEN_MARKS.has(name)) {
          if (!isActive(node.from)) decos.push(hide.range(node.from, node.to));
          return;
        }

        if (name === 'InlineCode') {
          if (isActive(node.from)) return;
          const marks = node.node.getChildren('CodeMark');
          for (const m of marks) decos.push(hide.range(m.from, m.to));
          return false;
        }

        if (name === 'Link') {
          const url = node.node.getChild('URL');
          const marks = node.node.getChildren('LinkMark');
          if (!url || marks.length < 2 || isActive(node.from)) return false;
          const textFrom = marks[0].to;
          const textTo = marks[1].from;
          decos.push(hide.range(node.from, textFrom));
          if (textTo > textFrom) {
            decos.push(Decoration.mark({
              class: 'cm-link-text',
              attributes: { 'data-href': doc.sliceString(url.from, url.to), title: doc.sliceString(url.from, url.to) },
            }).range(textFrom, textTo));
          }
          decos.push(hide.range(textTo, node.to));
          return false;
        }

        if (name === 'FencedCode') {
          const first = doc.lineAt(node.from).number;
          const last = doc.lineAt(node.to).number;
          for (let n = first; n <= last; n++) {
            const edge = n === first ? ' cm-code-first' : n === last ? ' cm-code-last' : '';
            decos.push(line('cm-code' + edge).range(doc.line(n).from));
          }
          return;
        }

        if (name === 'Blockquote') {
          const first = doc.lineAt(node.from).number;
          const last = doc.lineAt(node.to).number;
          for (let n = first; n <= last; n++) decos.push(line('cm-quote').range(doc.line(n).from));
          return;
        }

        if (name === 'ListMark') {
          const parentList = node.node.parent?.parent?.name;
          if (parentList === 'BulletList' && !isActive(node.from)) decos.push(bullet.range(node.from, node.to));
          return;
        }

        if (name === 'HorizontalRule') {
          if (!isActive(node.from)) decos.push(rule.range(node.from, node.to));
          return;
        }

        if (name === 'Table') {
          const first = doc.lineAt(node.from).number;
          const last = doc.lineAt(node.to).number;
          for (let n = first; n <= last; n++) decos.push(line('cm-table').range(doc.line(n).from));
          return false;
        }
      },
    });
  }
  return Decoration.set(decos, true);
}

const livePreview = (frontmatterNote) => ViewPlugin.fromClass(
  class {
    constructor(view) {
      this.decorations = buildDecorations(view, frontmatterNote);
    }
    update(u) {
      if (u.docChanged || u.viewportChanged || u.selectionSet || u.focusChanged) {
        this.decorations = buildDecorations(u.view, frontmatterNote);
      }
    }
  },
  { decorations: (v) => v.decorations },
);

const markdownStyle = HighlightStyle.define([
  { tag: tags.heading, fontWeight: '680', color: 'var(--text)' },
  { tag: tags.strong, fontWeight: '650' },
  { tag: tags.emphasis, fontStyle: 'italic' },
  { tag: tags.strikethrough, textDecoration: 'line-through', color: 'var(--text-3)' },
  { tag: tags.link, color: 'var(--accent)' },
  { tag: tags.url, color: 'var(--text-3)' },
  { tag: tags.monospace, class: 'cm-mono' },
  { tag: tags.processingInstruction, color: 'var(--text-3)' },
  { tag: tags.quote, color: 'var(--text-2)' },
  { tag: tags.contentSeparator, color: 'var(--text-3)' },
  { tag: [tags.comment, tags.meta], color: 'var(--text-3)' },
]);

function openLinkOnClick(onOpenLink) {
  return EditorView.domEventHandlers({
    mousedown(event) {
      const link = event.target.closest?.('.cm-link-text');
      if (!link || event.button !== 0) return false;
      event.preventDefault();
      onOpenLink(link.dataset.href);
      return true;
    },
  });
}

// Creates an editor. Markdown files get live preview; anything else edits as plain
// monospace text. A frontmatterNote element is shown at the end of the frontmatter.
export function createEditor(parent, { doc, isMarkdown, onChange, onOpenLink, frontmatterNote }) {
  const extensions = [
    history(),
    drawSelection(),
    keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
    EditorView.lineWrapping,
    placeholder('Empty file'),
    EditorView.updateListener.of((u) => {
      if (u.docChanged && !u.transactions.some((t) => t.annotation(fromDisk))) onChange(u.state.doc.toString());
    }),
  ];
  if (isMarkdown) {
    extensions.push(
      markdown({ base: markdownLanguage }),
      syntaxHighlighting(markdownStyle),
      livePreview(frontmatterNote),
      openLinkOnClick(onOpenLink),
    );
  }
  return new EditorView({ parent, state: EditorState.create({ doc, extensions }) });
}

// Replaces only the changed middle of the document, so the cursor and scroll
// position survive a reload from disk.
export function replaceContent(view, text) {
  const old = view.state.doc.toString();
  if (old === text) return;
  let start = 0;
  while (start < old.length && start < text.length && old[start] === text[start]) start++;
  let endOld = old.length;
  let endNew = text.length;
  while (endOld > start && endNew > start && old[endOld - 1] === text[endNew - 1]) {
    endOld--;
    endNew--;
  }
  view.dispatch({
    changes: { from: start, to: endOld, insert: text.slice(start, endNew) },
    annotations: fromDisk.of(true),
  });
}
