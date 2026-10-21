# Wakeshift connectors

The open-source parts of [Wakeshift](https://wakeshift.ai) that run on your machine or inside your agent. Wakeshift wakes AI agents (Claude Code, Codex, Cursor and others) when something happens in the apps they're connected to; this is the code that does it on your side, published so you can read exactly what it does.

| Path | What it is |
| --- | --- |
| `cmd/wakeshift-connector` | The `wakeshift` program: the hooks and connectors that let Wakeshift wake an agent, and its install and uninstall commands. |
| `internal/` | The packages compiled into it: reading Wakeshift's signed beacons from tool results, holding a conversation's stream link, framing messages as outside information, each harness's setup. |
| `plugins/hermes` | Wakeshift's plugin for [Hermes Agent](https://hermes-agent.nousresearch.com) (Python). |
| `plugins/openclaw` | Wakeshift's plugin for [OpenClaw](https://docs.openclaw.ai) (TypeScript). |

The TypeScript SDK for building apps is at [wakeshift/wakeshift-node](https://github.com/wakeshift/wakeshift-node).

## Building and testing

```sh
go build -o wakeshift ./cmd/wakeshift-connector
go test ./...

cd plugins/hermes && uv run --no-project --with cryptography python -m unittest discover -s tests -t tests
cd plugins/openclaw && npm ci && npm run lint && npm test
```

To stamp a version: `go build -ldflags "-X github.com/wakeshift/connectors/internal/version.Version=<version>" ./cmd/wakeshift-connector`.

The Hermes plugin's end-to-end test needs a running Wakeshift, so it runs in Wakeshift's own repository.

## Where changes happen

This repository is generated from Wakeshift's main repository. Pull requests are welcome: we apply them there and publish the result. Each commit here says which commit it came from (`.source-commit`); see [CONTRIBUTING](CONTRIBUTING.md).

## License

Apache License 2.0; see [LICENSE](LICENSE) and [NOTICE](NOTICE).
