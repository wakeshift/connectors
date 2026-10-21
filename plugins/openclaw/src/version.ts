// The plugin's version, which every request to Wakeshift names
// (internal/connectorversion), so Wakeshift records what's in use and can
// ask the user to update. "dev" here; `npm run pack` stamps the build's
// version into dist/version.js (scripts/stamp-version.mjs, from
// WAKESHIFT_VERSION).
export const VERSION: string = "dev";

export const CLIENT_HEADER = "X-Wakeshift-Connector";

/** X-Wakeshift-Connector's value: wakeshift-openclaw/<version> openclaw. */
export function clientHeader(version: string = VERSION): string {
  const v = /^[0-9A-Za-z][0-9A-Za-z.+_-]{0,63}$/.test(version) ? version : "0";
  return `wakeshift-openclaw/${v} openclaw`;
}
