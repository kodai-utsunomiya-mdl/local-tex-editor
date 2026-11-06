import { build } from 'esbuild';
import { cp, mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import path from 'node:path';

const root = path.resolve(import.meta.dirname, '..');
const out = path.join(root, 'web/dist');
await rm(out, { recursive: true, force: true });
await mkdir(out, { recursive: true });
await writeFile(path.join(out, '.keep'), '');
await build({
  entryPoints: [path.join(root, 'web/src/main.js')],
  outfile: path.join(out, 'app.js'),
  bundle: true,
  minify: true,
  format: 'esm',
  target: ['chrome120', 'edge120', 'firefox128'],
  legalComments: 'linked',
  logLevel: 'info'
});
await cp(path.join(root, 'web/src/index.html'), path.join(out, 'index.html'));
await cp(path.join(root, 'internal/branding/app-icon.svg'), path.join(out, 'app-icon.svg'));
await cp(path.join(root, 'web/src/style.css'), path.join(out, 'style.css'));
await cp(path.join(root, 'web/src/pdf-search.css'), path.join(out, 'pdf-search.css'));
await cp(path.join(root, 'web/src/source-search.css'), path.join(out, 'source-search.css'));
const pdf = path.join(root, 'node_modules/pdfjs-dist');
await cp(path.join(pdf, 'build/pdf.worker.min.mjs'), path.join(out, 'pdf.worker.min.mjs'));
await cp(path.join(pdf, 'web/pdf_viewer.css'), path.join(out, 'pdf_viewer.css'));
await cp(path.join(pdf, 'web/images'), path.join(out, 'images'), { recursive: true });
for (const folder of ['cmaps', 'standard_fonts', 'wasm']) {
  await cp(path.join(pdf, folder), path.join(out, folder), { recursive: true });
}
// Keep the third-party license text with the assets embedded in the executable.
let notices = '# Third-party licenses\n\n';
for (const packageName of ['codemirror','@codemirror/state','@codemirror/view','@codemirror/commands','@codemirror/language','@codemirror/search','@codemirror/autocomplete','@codemirror/legacy-modes','@codemirror/lint','@codemirror/streamparser','@lezer/common','@lezer/highlight','@lezer/lr','style-mod','w3c-keyname','crelt','@marijn/find-cluster-break','pdfjs-dist']) {
  const dir = path.join(root,'node_modules',packageName);
  const metadata = JSON.parse(await readFile(path.join(dir,'package.json'),'utf8'));
  const license = await readFile(path.join(dir,'LICENSE'),'utf8');
  notices += `## ${packageName} ${metadata.version}\n\n${license}\n\n`;
}
await writeFile(path.join(out, 'THIRD_PARTY_LICENSES.txt'), notices, 'utf8');
