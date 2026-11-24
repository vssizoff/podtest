const katex = require('katex');
const fs = require('node:fs');
const input = fs.readFileSync(0, 'utf8');
const render = (source, displayMode) => katex.renderToString(source, {
  displayMode, throwOnError: true, strict: 'error', trust: false,
  maxExpand: 100, maxSize: 16, output: 'htmlAndMathml',
});
if (/\\(?:href|url|html\w*|includegraphics|input|include|write|read|usepackage|openout|def|gdef|edef|xdef|newcommand|renewcommand)\b/.test(input)) {
  throw new Error('Unsupported TeX command');
}
if (process.argv[2] === 'latex') {
  process.stdout.write(render(input, true));
} else {
  const entities = {amp: '&', lt: '<', gt: '>', quot: '"', '#39': "'"};
  const decode = (text) => text.replace(/&(amp|lt|gt|quot|#39);/g, (_, name) => entities[name]);
  const tokens = input.split(/(<pre\b[^>]*>[\s\S]*?<\/pre>|<code\b[^>]*>[\s\S]*?<\/code>|<!--[\s\S]*?-->|<[^>]+>)/gi);
  const output = tokens.map((token, index) => index % 2 ? token : token.replace(/(?<!\\)\$\$([^$]+)\$\$|(?<!\\)\$([^\n$]+)\$/g,
    (_, display, inline) => render(decode(display ?? inline), display !== undefined))).join('');
  process.stdout.write(output);
}
