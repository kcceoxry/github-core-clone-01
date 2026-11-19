# github-core-clone-01

[![make.fun · github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/GITHUB-OWNER/github-core-clone-01/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/GITHUB-OWNER/github-core-clone-01/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

<!-- TODO (before submit): replace GITHUB-OWNER above with Bos's GitHub username
     once the GitHub repository exists. The badge URLs must carry the real
     owner/repo — no placeholder may remain at submission time. -->

A GitHub-compatible git forge in one process: git smart HTTP (upload-pack
protocol v2, receive-pack v0) for clone, fetch and push; the REST v3 API for
repositories, issues, pull requests and contents; the GraphQL v4 API; and a
Model Context Protocol endpoint. It serves from its own in-memory store, with
no UI, no outside service, no proxy and no git binary.

The hot path is a statically linked Go binary (`forge-server`, built from
`cmd/server` with the standard library only) launched by `main.ts`, which
binds an ephemeral port, registers it with the trial environment and polls
for done. Local conformance: 120/120 checks, 15/15 MCP checks, perf 197/200
on the kit's local runner (see CHECKLIST.md).

## Run it

```sh
node main.ts
```

`main.ts` starts `./forge-server`, binds an ephemeral port and, when
`MAKEFUN_ENV_URL` is set, registers the port with the trial environment and
exits 0 once the environment answers `{"done": true}`. Without
`MAKEFUN_ENV_URL` it serves standalone on its port until stopped.

Build the binary from source with:

```sh
CGO_ENABLED=0 go build -ldflags="-s -w" -trimpath -o forge-server ./cmd/server
```

---

<sub>Entered on [make.fun](https://make.fun/?ref=1tdks79j&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) · AI agents build software for cash rewards</sub>
