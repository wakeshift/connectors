#!/bin/sh
# Checks the packed SDK the way an app installs it: npm pack, then a fresh
# project per zod major (3 and 4) and MCP SDK (the oldest the peer range
# allows, and the latest) installs the tarball, typechecks a server with
# tsc --strict, and runs it. Needs the npm registry.
set -eu
sdk=$(cd "$(dirname "$0")/.." && pwd)
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT

cd "$sdk"
tgz="$dir/$(npm pack --silent --pack-destination "$dir" | tail -1)"

mcp_floor=$(node -p 'require("./package.json").peerDependencies["@modelcontextprotocol/sdk"].replace(/^\^/, "")')
for zod in 3 4; do
for mcp in "$mcp_floor" latest; do
  app="$dir/app-zod$zod-mcp$mcp"
  mkdir -p "$app"
  cp "$sdk/scripts/pack-check/server.ts" "$app/"
  cd "$app"
  printf '{"name":"pack-check","private":true,"type":"module"}\n' >package.json
  npm install --silent --no-fund --no-audit "$tgz" "zod@^$zod" "@modelcontextprotocol/sdk@$mcp" typescript@5 @types/node@22 >/dev/null
  if [ -d node_modules/@wakeshift/sdk/node_modules ]; then
    echo "zod $zod: the tarball brought its own dependencies: $(ls node_modules/@wakeshift/sdk/node_modules)" >&2
    exit 1
  fi
  npx tsc --strict --noEmit --module nodenext --moduleResolution nodenext --target es2023 --types node --skipLibCheck server.ts
  node --experimental-strip-types --no-warnings server.ts
  echo "zod $zod ($(node -p 'require("zod/package.json").version')), MCP SDK $(node -p 'require("@modelcontextprotocol/sdk/package.json").version'): ok"
done
done
