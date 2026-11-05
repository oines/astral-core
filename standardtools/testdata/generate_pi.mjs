// Optional maintainer tool: regenerate semantic fixtures from an audited local
// Pi checkout. Go tests need neither Node nor Pi. No checkout files are changed.
import { readFile, writeFile } from 'node:fs/promises';
import { stripTypeScriptTypes } from 'node:module';
import { resolve, dirname } from 'node:path';

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
  ['multi missing', 'alpha\nbeta\n', [{oldText:'alpha',newText:'A'},{oldText:'missing',newText:'B'}]],
  ['multi duplicate', 'alpha\none\none\n', [{oldText:'alpha',newText:'A'},{oldText:'one',newText:'B'}]],
  ['multi empty', 'alpha\nbeta\n', [{oldText:'alpha',newText:'A'},{oldText:'',newText:'B'}]],
  ['multi no change', 'alpha\nbeta\n', [{oldText:'alpha',newText:'alpha'},{oldText:'beta',newText:'beta'}]],
  ['fuzzy whitespace-only range', 'ﬁ\n \n', [{oldText:'  ',newText:'changed'}]],
];
const edits = cases.map(([name, original, edits]) => {
  try { return { name, original, edits, expected: edit.applyEditsToNormalizedContent(original, edits, 'fixture.txt').newContent }; }
  catch (e) { return { name, original, edits, error: e.message }; }
});
const texts = [ '', 'one\ntwo\n', '\n', 'x\n'.repeat(2001), '字'.repeat(18000), 'x'.repeat(51200)+'\n', ('字'.repeat(1000)+'\n').repeat(20) ];
const truncations = texts.map((text, i) => ({name:'truncate-'+i, text, expected:truncate.truncateHead(text)}));
await writeFile(new URL('./pi_edits.json', import.meta.url), JSON.stringify(edits, null, 2)+'\n');
await writeFile(new URL('./pi_truncate.json', import.meta.url), JSON.stringify(truncations, null, 2)+'\n');

// Load only the audited file-tool definitions, with a tiny TypeBox schema
// builder and no Pi packages. Text reads use injected IO; no real file is read
// by the tools and no command/image/diff implementation is substituted for an
// assertion. This captures Pi's entire read Content, schemas and edit preparation.
const optional = Symbol('optional');
const Type = {
  String: (options = {}) => ({ type: 'string', ...options }),
  Number: (options = {}) => ({ type: 'number', ...options }),
  Literal: value => ({ const: value }),
  Union: values => ({ anyOf: values }),
  Array: (items, options = {}) => ({ type: 'array', ...options, items }),
  Optional: schema => Object.assign(schema, { [optional]: true }),
  Object: (properties, options = {}) => ({ type: 'object', ...options, properties,
    required: Object.entries(properties).filter(([, value]) => !value[optional]).map(([key]) => key) }),
};
const dependencies = {
  Type, constants: { R_OK: 4, W_OK: 2 }, dirname,
  fsAccess() {}, fsReadFile() {}, fsWriteFile() {}, fsMkdir() {},
  detectSupportedImageMimeTypeFromFile() {},
  readRenderers: {}, writeRenderers: {}, editRenderers: {},
  resolveReadPathAsync: async path => path, resolveToCwd: path => path,
  withFileMutationQueue: async (_path, work) => work(),
  ...truncate, ...edit,
};
async function fileToolModule(name) {
  const source = await readFile(resolve(repo, `packages/coding-agent/src/core/tools/${name}.ts`), 'utf8');
  const js = stripTypeScriptTypes(source.replace(/^import[\s\S]*?;\r?\n/gm, '')).replace(/^export /gm, '');
  const title = name[0].toUpperCase() + name.slice(1);
  const extra = name === 'edit' ? ', prepareEditArguments' : '';
  return new Function(...Object.keys(dependencies), js + `\nreturn { create: create${title}ToolDefinition${extra} };`)(...Object.values(dependencies));
}
const readModule = await fileToolModule('read');
const writeModule = await fileToolModule('write');
const editModule = await fileToolModule('edit');
const declarations = [readModule, writeModule, editModule].map(module => {
  const tool = module.create('/fixture');
  return { name: tool.name, description: tool.description, guidelines: tool.promptGuidelines, parameters: tool.parameters };
});
await writeFile(new URL('./pi_file_definitions.json', import.meta.url), JSON.stringify(declarations, null, 2)+'\n');
const readCases = [
  ['empty', '', {}], ['full trailing newline', '\ufeffone\r\ntwo\r\n', {}],
  ['first zero', 'one\ntwo\nthree', {offset:0,limit:1}],
  ['first negative', 'one\ntwo\nthree', {offset:-3,limit:1}],
  ['empty limit', 'one\ntwo\nthree', {offset:2,limit:0}],
  ['fractional', 'one\ntwo\nthree', {offset:1.5,limit:1.5}],
  ['negative limit', 'one\ntwo\nthree', {limit:-1}],
  ['beyond', 'one\ntwo\n', {offset:4}],
  ['large offset number', 'one', {offset:1000000}],
  ['lines', 'line\n'.repeat(2001), {}],
  ['byte boundary', ('字'.repeat(1000)+'\n').repeat(20), {}],
  ['invalid utf8', Buffer.from([0xff,0xff,0xe2,0x82,0x20,0xf0,0x9f,0x8c]), {}],
];
const readResults = [];
for (const [name, text, options] of readCases) {
  const data = Buffer.from(text);
  const tool = readModule.create('/fixture', {operations:{access:async()=>{}, readFile:async()=>data}});
  const input = {path:'fixture.txt', ...options};
  try { const result = await tool.execute('fixture', input); readResults.push({name, base64:data.toString('base64'), input, content:result.content, details:result.details}); }
  catch(error) { readResults.push({name, base64:data.toString('base64'), input, error:error.message}); }
}
await writeFile(new URL('./pi_reads.json', import.meta.url), JSON.stringify(readResults, null, 2)+'\n');
const normalizations = [
  {path:'f',oldText:'a',newText:'b'},
  {path:'f',edits:{oldText:'a',newText:'b'}},
  {path:'f',edits:JSON.stringify([{oldText:'a',newText:'b'}])},
  {path:'f',edits:JSON.stringify({oldText:'a',newText:'b'})},
  {path:'f',edits:[{oldText:'a',newText:'b'}],oldText:'c',newText:'d'},
  {path:'f',edits:'malformed',oldText:'a',newText:'b'},
  {path:'f',edits:null,oldText:'a',newText:'b'},
  {path:'f',edits:'malformed'}, {path:'f',edits:{oldText:'a'}},
].map(input => ({input, expected:editModule.prepareEditArguments(structuredClone(input))}));
await writeFile(new URL('./pi_edit_arguments.json', import.meta.url), JSON.stringify(normalizations, null, 2)+'\n');
