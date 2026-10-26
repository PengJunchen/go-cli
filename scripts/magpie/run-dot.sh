#!/usr/bin/env bash
set -euo pipefail

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd -- "${script_dir}/../.." && pwd)"
binary="${repo_root}/bin/go-cli"

if [[ ! -x "${binary}" ]]; then
	printf 'go-cli binary is missing or not executable: %s\n' "${binary}" >&2
	printf 'Build the project binary before using this launcher.\n' >&2
	exit 1
fi

export GO_CLI_PROVIDER_NAME="openai"
export GO_CLI_PROVIDER_BASE_URL="http://127.0.0.1:3425/v1"
export GO_CLI_PROVIDER_API_KEY="magpie"
export GO_CLI_PROVIDER_MODEL="codex/gpt-6.1-sol"
export GO_CLI_MODEL_NAME="codex/gpt-6.1-sol"

: "${GO_CLI_PRODUCTION_MODEL_TIMEOUT_TOTAL:=10m}"
: "${GO_CLI_PRODUCTION_MODEL_TIMEOUT_STREAM_IDLE:=4m}"
export GO_CLI_PRODUCTION_MODEL_TIMEOUT_TOTAL
export GO_CLI_PRODUCTION_MODEL_TIMEOUT_STREAM_IDLE

if (($# == 0)); then
	set -- interactive
fi

exec "${binary}" "$@"
