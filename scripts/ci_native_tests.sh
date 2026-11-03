#!/usr/bin/env bash
set -euo pipefail
case "${1:?native suite required}" in
  filesystem)
    # The lower-level FS/spool/provider contracts run on every host. At the
    # Agent layer select OS operations rather than common transport/policy tests.
    go run ./internal/citest -log=test-results/native-libraries.jsonl -- \
      ./internal/agent/filesystem ./internal/agent/artifacts ./internal/agent/providers ./internal/agent/hostservices ./cmd/agent
    go run ./internal/citest -package=./internal/agent \
      -files='(file|recover|logging|input_open|receive_buffer|code_fault|cli_validate|preflight).*_test.go$' \
      -log=test-results/native-agent.jsonl
    ;;
  author-cli)
    go run ./internal/citest -package=./internal/agent -include='^TestCodeCLIRealAuthorWorkflow' -log=test-results/native-cli.jsonl
    ;;
  wasm-[12])
    # Exercise real WASI filesystem semantics and the host's process/resource
    # enforcement. Common SDK parsing/ABI matrices run once in Linux CI.
    go run ./internal/citest -package=./internal/agent/wasmruntime \
      -files='^(wasi_.*|worker(_cpu_limit_linux)?)_test.go$' \
      -exclude='^TestWasmRealWorker(TerminalErrors|EnvelopeBoundsAndCommonOutput|LogsDrainBudgetAndTail|ProjectionLimitsAndReclamation)$' \
      -shard="${1#wasm-}" -shards=2 -log=test-results/native-wasm.jsonl
    ;;
  *) echo "unknown native suite: $1" >&2; exit 1 ;;
esac
