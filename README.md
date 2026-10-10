# shy

A coding agent in one bash file (`shy.sh`). It has no package manifest. These are the dependencies the script actually uses.

## Required

shy exits at startup if any of these are missing.

| Dependency | Notes |
| --- | --- |
| [bash](https://www.gnu.org/software/bash/) 3.2+ | Interpreter (`#!/usr/bin/env bash`). JSON is parsed in bash, so [jq](https://jqlang.org/) is not used. |
| [curl](https://curl.se/) | OpenRouter chat completions, the `webfetch` tool, and optional Neatlogs ingest. Checked with `command -v curl`. |
| `mktemp` | Creates the working directory (`mktemp -d`). Startup fails if that directory cannot be created. |
| `/usr/bin/env` | Resolves `bash` from the shebang. |

## Commands the script calls

These come from a normal Unix userland (macOS or GNU). shy does not install them.

| Command | Used for |
| --- | --- |
| `sleep` | Spinner (`sleep 0.1`), kill delays, and request retries. Fractional seconds are required. |
| `mkdir` | `mkdir -p` when the `write` tool needs a parent directory. |
| `rm` | Deletes temp files and the working directory on exit (`rm -f`, `rm -rf`). |
| `uname` | `uname -sm` for the operating-system line in the system prompt. |
| `ps` | `ps -A -o pid= -o ppid=` so a timed-out or interrupted command can be killed with its children. |
| `date` | `date -u '+%s …'` for timestamps when bash has no `EPOCHREALTIME` (bash before 5). |
| `cat` | Prints `--help`. Also set as `PAGER` and `GIT_PAGER` for commands the `bash` tool runs. |

`printf`, `kill`, `wait`, `read`, `disown`, `history`, and `trap` are bash builtins, not separate programs.

Interactive mode also needs a terminal on `/dev/tty`. One-shot mode (`shy.sh "task"`) does not.

## Services

| Service | Required | Used for |
| --- | --- | --- |
| [OpenRouter](https://openrouter.ai/) | yes | Model calls. Set `OPENROUTER_API_KEY`. Default model: `deepseek/deepseek-v4.1-flash` (`SHY_MODEL` or `-m`). Endpoint: `$OPENROUTER_BASE_URL` or `https://openrouter.ai/api/v1`. |
| [Neatlogs](https://neatlogs.com/) | no | Tracing. Off unless `NEATLOGS_API_KEY` is set. Ingest: `$NEATLOGS_INGEST_URL` or `https://ingest.neatlogs.com`. Write keys (`nlw_…`) also need `NEATLOGS_PROJECT_NAME`. |
