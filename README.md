# shy

<img src="logo.svg" alt="shy" width="180">

A coding agent in one bash file (`shy.sh`). It has no package manifest. These are the dependencies the script actually uses.

## Required

shy exits at startup if any of these are missing.

| Dependency | Notes |
| --- | --- |
| [bash](https://www.gnu.org/software/bash/) 3.2+ | Interpreter (`#!/usr/bin/env bash`). JSON is parsed in bash, so [jq](https://jqlang.org/) is not used. |
| [curl](https://curl.se/) | Model calls, the `webfetch` tool, and optional Neatlogs ingest. Checked with `command -v curl`. |
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
| shyd | yes | Model calls, and the trace at the end of a run. Set `SHY_API_KEY` to an agent key from the dashboard. Default origin: `http://127.0.0.1:8080` (`SHY_BASE_URL`). shyd forwards the model call to OpenRouter and stores the trace. Default model: `deepseek/deepseek-v4.1-flash` (`SHY_MODEL` or `-m`). |
| OpenAI-compatible API | no | Set `OPENAI_BASE_URL` and `OPENAI_API_KEY` to send model calls there instead of shyd. `OPENAI_BASE_URL` is the API root, for example `https://openrouter.ai/api/v1` or `https://api.openai.com/v1`. With `SHY_API_KEY` set and `NEATLOGS_API_KEY` unset, traces still go to shyd. |
| [Neatlogs](https://neatlogs.com/) | no | Direct tracing, instead of shyd. Off unless `NEATLOGS_API_KEY` is set. Ingest: `$NEATLOGS_INGEST_URL` or `https://ingest.neatlogs.com`. Write keys (`nlw_…`) also need `NEATLOGS_PROJECT_NAME`. |
