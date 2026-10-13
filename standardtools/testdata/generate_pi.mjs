// Optional maintainer tool: regenerate semantic fixtures from an audited local
// Pi checkout. Go tests need neither Node nor Pi. No checkout files are changed.
import { readFile, writeFile } from 'node:fs/promises';
import { stripTypeScriptTypes } from 'node:module';
import { resolve } from 'node:path';

const repo = process.argv[2];
if (!repo) throw new Error('usage: node generate_pi.mjs /path/to/pi-mono');
async function pureModule(file) {
  const source = await readFile(resolve(repo, file), 'utf8');
  // These two audited source files have only single-line imports. The selected
  // pure exports do not call filesystem or diff-library functions.
  const stripped = stripTypeScriptTypes(source.replace(/^import .*;\r?\n/gm, ''));
  return import('data:text/javascript;base64,' + Buffer.from(stripped).toString('base64'));
}
const edit = await pureModule('packages/coding-agent/src/core/tools/edit-diff.ts');
const truncate = await pureModule('packages/coding-agent/src/core/tools/truncate.ts');
const cases = [
  ['disjoint original ranges', 'alpha\nbeta\ngamma\n', [{oldText:'alpha',newText:'beta'},{oldText:'beta',newText:'delta'}]],
  ['overlap', 'abcdef\n', [{oldText:'abcd',newText:'x'},{oldText:'cdef',newText:'y'}]],
  ['ambiguous', 'one\none\n', [{oldText:'one',newText:'two'}]],
  ['normalized ambiguous', 'x “q”\nx "q"\n', [{oldText:'x "q"',newText:'changed'}]],
  ['missing', 'alpha\n', [{oldText:'missing',newText:'new'}]],
  ['no change', 'alpha\n', [{oldText:'alpha',newText:'alpha'}]],
  ['empty', 'alpha\n', [{oldText:'',newText:'alpha'}]],
  ['fuzzy preserve untouched', 'keep “quotes”   \nconst x = “old”;  \nkeep — dash\n', [{oldText:'const x = "old";',newText:'const x = "new";'}]],
  ['mixed exact and fuzzy', 'ﬁrst\nexact\nkeep  \n', [{oldText:'first',newText:'first!'},{oldText:'exact',newText:'exact!'}]],
  ['two changes on normalized line', '“alpha” — beta\nuntouched  \n', [{oldText:'"alpha"',newText:'A'},{oldText:'beta',newText:'B'}]],
  ['multiline newline end', 'head\nﬁrst\nsecond\ntail\n', [{oldText:'first\nsecond\n',newText:'new\n'}]],
  ['unicode offsets', '你好 🪐\nconst n = 1\n', [{oldText:'🪐',newText:'星球'},{oldText:'n = 1',newText:'n = 2'}]],
  ['crlf arguments', 'a\nb\nc\n', [{oldText:'a\r\nb',newText:'x\r\ny'}]],
  ['NEL is not JS whitespace', 'x\u0085\nﬁrst\n', [{oldText:'first',newText:'new'}]],
];
const edits = cases.map(([name, original, edits]) => {
  try { return { name, original, edits, expected: edit.applyEditsToNormalizedContent(original, edits, 'fixture.txt').newContent }; }
  catch (e) { return { name, original, edits, error: e.message }; }
});
const texts = [ '', 'one\ntwo\n', '\n', 'x\n'.repeat(2001), '字'.repeat(18000), 'x'.repeat(51200)+'\n', ('字'.repeat(1000)+'\n').repeat(20) ];
const truncations = texts.map((text, i) => ({name:'truncate-'+i, text, expected:truncate.truncateHead(text)}));
await writeFile(new URL('./pi_edits.json', import.meta.url), JSON.stringify(edits, null, 2)+'\n');
await writeFile(new URL('./pi_truncate.json', import.meta.url), JSON.stringify(truncations, null, 2)+'\n');
