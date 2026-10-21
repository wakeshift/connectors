# Wakeshift plugins

The open-source parts of [Wakeshift](https://wakeshift.ai), which wakes AI agents (Claude Code, Codex, Cursor and others) when something happens in the apps they're connected to. This is the code that runs on your machine or inside your agent, published so you can read exactly what it does.

| Path | What it is |
| --- | --- |
| `cmd/wakeshift-connector` | The `wakeshift` program: the hooks and connectors that let Wakeshift wake an agent, and its install and uninstall commands. |
| `internal/` | The packages compiled into it: reading Wakeshift's signed beacons from tool results, holding a conversation's stream link, framing messages as outside information, each harness's setup. |
| `plugins/hermes` | Wakeshift's plugin for [Hermes Agent](https://hermes-agent.nousresearch.com) (Python). |
| `plugins/openclaw` | Wakeshift's plugin for [OpenClaw](https://docs.openclaw.ai) (TypeScript). |
| `sdk/typescript` | `@wakeshift/sdk`, for building apps that wake agents. |

## Building and testing

```sh
go build -o wakeshift ./cmd/wakeshift-connector
go test ./...

cd plugins/hermes && uv run --no-project --with cryptography python -m unittest discover -s tests -t tests
cd plugins/openclaw && npm ci && npm run lint && npm test
cd sdk/typescript && npm ci && WAKESHIFT_EXAMPLES_README_ONLY=1 npm test
```

To stamp a version: `go build -ldflags "-X github.com/clawvisor/wakeshift-plugins/internal/version.Version=<version>" ./cmd/wakeshift-connector`.

Tests that need a running Wakeshift (the SDK's and Hermes plugin's end-to-end tests) run in Wakeshift's own repository.

## Where changes happen

This repository is published from Wakeshift's main repository, where this code is developed and tested together with the server it talks to. Each commit here says which commit it came from (`.source-commit`). Issues and pull requests are welcome; accepted changes are made upstream and arrive here with the next publish.

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
