// Stamps the build's version (WAKESHIFT_VERSION, as `make connector` and
// the image build set it) into dist/version.js, after `npm run build`.
// Without it the plugin says "dev" (version 0 to Wakeshift).
import { readFileSync, writeFileSync } from "node:fs";

const v = process.env.WAKESHIFT_VERSION ?? "";
if (!v) process.exit(0);
if (!/^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$/.test(v)) {
  console.error(`WAKESHIFT_VERSION isn't a version: ${v}`);
  process.exit(1);
}
const f = new URL("../dist/version.js", import.meta.url);
const js = readFileSync(f, "utf8");
const at = /export const VERSION = "dev";/;
if (!at.test(js)) {
  console.error("dist/version.js has no VERSION to stamp");
  process.exit(1);
}
writeFileSync(f, js.replace(at, `export const VERSION = ${JSON.stringify(v)};`));
