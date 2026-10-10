#!/usr/bin/env bash
# shy: a coding agent in one bash file. Needs bash (3.2+) and curl. No jq.
#
#   curl -fsSL <url>/shy.sh | bash                          interactive session
#   curl -fsSL <url>/shy.sh | bash -s -- "fix the tests"    one-shot task
#   bash shy.sh [-m model] [task...]
#
# Environment:
#   OPENROUTER_API_KEY     OpenRouter key (asked for when unset)
#   SHY_MODEL              model id (default: anthropic/claude-sonnet-5.5)
#   SHY_MAX_DEPTH          how deep subagents may nest (default: 2)
#   SHY_MAX_STEPS          model calls per agent run (default: 100)
#   SHY_BASH_TIMEOUT       default bash tool timeout in seconds (default: 300)
#   NEATLOGS_API_KEY       turns on tracing to Neatlogs
#   NEATLOGS_PROJECT_NAME  dashboard project name, required with nlw_ write keys
#   NEATLOGS_INGEST_URL    default https://ingest.neatlogs.com (EU: https://eu.ingest.neatlogs.com)
#   NEATLOGS_SESSION_ID    session id for traces (interactive mode makes one up)
#   NEATLOGS_END_USER_ID   end user id for traces

if [ -z "${BASH_VERSION:-}" ]; then echo "shy: run me with bash" >&2; exit 1; fi

SHY_VERSION=0.1.0

# ---------------------------------------------------------------- setup

setup() {
  MODEL=${SHY_MODEL:-anthropic/claude-sonnet-5.5}
  MAX_DEPTH=${SHY_MAX_DEPTH:-2}
  MAX_STEPS=${SHY_MAX_STEPS:-100}
  BASH_TIMEOUT=${SHY_BASH_TIMEOUT:-300}
  OPENROUTER_URL=${OPENROUTER_BASE_URL:-https://openrouter.ai/api/v1}
  NEATLOGS_INGEST_URL=${NEATLOGS_INGEST_URL:-https://ingest.neatlogs.com}
  MAX_BYTES=51200
  MAX_LINES=2000
  TRACE_INPUT_MAX=200000

  # Byte semantics keep string lengths, offsets and pattern matching linear.
  ORIG_LC_ALL_SET=${LC_ALL+1}
  ORIG_LC_ALL=${LC_ALL:-}
  LC_ALL=C
  shopt -u patsub_replacement 2>/dev/null

  OLD_BASH=
  if ((BASH_VERSINFO[0] < 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] < 3))); then OLD_BASH=1; fi

  C_RST= C_DIM= C_BOLD= C_RED= C_GRN= C_YEL= C_CYN=
  if [[ -t 2 && -z ${NO_COLOR:-} ]]; then
    C_RST=$'\e[0m' C_DIM=$'\e[2m' C_BOLD=$'\e[1m' C_RED=$'\e[31m' C_GRN=$'\e[32m' C_YEL=$'\e[33m' C_CYN=$'\e[36m'
  fi
  SPIN=('⠋' '⠙' '⠹' '⠸' '⠼' '⠴' '⠦' '⠧' '⠇' '⠏')
  UI_PFX=
  HAVE_TTY=
  { : </dev/tty; } 2>/dev/null && HAVE_TTY=1

  INTERRUPTED= CHILD_PID= IN_TURN=
  OS_NAME=$(uname -sm 2>/dev/null)

  json_setup
  tools_setup
}

locale_user() {
  if [[ -n $ORIG_LC_ALL_SET ]]; then LC_ALL=$ORIG_LC_ALL; else unset LC_ALL; fi
}

# ---------------------------------------------------------------- ui

die() { printf '%sshy: %s%s\n' "$C_RED" "$*" "$C_RST" >&2; exit 1; }
warn() { printf '%s%s%s%s\n' "$UI_PFX" "$C_YEL" "$*" "$C_RST" >&2; }
note() { printf '%s%s%s%s\n' "$UI_PFX" "$C_DIM" "$*" "$C_RST" >&2; }

# Byte slicing can split a UTF-8 character, and APIs reject invalid UTF-8.
# utf8_head: REPLY = $1 minus a partial character at the end.
utf8_head() {
  local s=$1 n c need
  for n in 1 2 3 4; do
    ((n > ${#s})) && break
    c=${s:${#s}-n:1}
    case $c in
      ["$U_CONT"]) continue ;;
      ["$U_LEAD2"]) need=2 ;;
      ["$U_LEAD3"]) need=3 ;;
      ["$U_LEAD4"]) need=4 ;;
      *) break ;;
    esac
    ((n < need)) && s=${s:0:${#s}-n}
    break
  done
  REPLY=$s
}

# utf8_tail: REPLY = $1 minus a partial character at the start.
utf8_tail() {
  local s=$1 n=0
  while ((n < 3)) && [[ $s == ["$U_CONT"]* ]]; do
    s=${s:1}
    n=$((n + 1))
  done
  REPLY=$s
}

# First line of $1, cut to $2 bytes.
oneline() {
  local s=${1%%$'\n'*} max=${2:-100}
  if ((${#s} > max)); then
    utf8_head "${s:0:max}"
    s="$REPLY…"
  elif [[ ${#s} -ne ${#1} ]]; then
    s+=" …"
  fi
  REPLY=$s
}

# Print up to $2 lines of $1, each prefixed with $3.
show_lines() {
  local s=$1 max=$2 pfx=$3 l i=0
  while [[ -n $s ]] && ((i < max)); do
    l=${s%%$'\n'*}
    if ((${#l} == ${#s})); then s=; else s=${s:${#l}+1}; fi
    ((${#l} > 200)) && utf8_head "${l:0:200}" && l="$REPLY…"
    printf '%s%s%s%s\n' "$pfx" "$C_DIM" "$l" "$C_RST" >&2
    i=$((i + 1))
  done
  [[ -n $s ]] && printf '%s%s…%s\n' "$pfx" "$C_DIM" "$C_RST" >&2
  return 0
}

# Like show_lines, but for the last $2 lines.
show_tail() {
  local s=$1 cut k=0
  while ((k < $2)); do
    cut=${s%$'\n'*}
    if ((${#cut} == ${#s})); then s= && break; fi
    s=$cut
    k=$((k + 1))
  done
  if [[ -n $s ]]; then
    printf '%s%s…%s\n' "$3" "$C_DIM" "$C_RST" >&2
    s=${1:${#s}+1}
  else
    s=$1
  fi
  show_lines "$s" "$2" "$3"
}

fmt_k() {
  if (($1 >= 1000)); then printf -v REPLY '%d.%dk' $(($1 / 1000)) $((($1 % 1000) / 100)); else REPLY=$1; fi
}

# Wait for a background pid with a spinner. Returns the pid's exit status.
spin_wait() {
  local pid=$1 label=$2 i=0 t0=$SECONDS rc tries=0
  CHILD_PID=$pid
  if [[ -t 2 ]]; then
    while [[ -z $INTERRUPTED ]] && kill -0 "$pid" 2>/dev/null; do
      printf '\r%s%s%s %s %ds%s\e[K' "$UI_PFX" "$C_CYN" "${SPIN[i % 10]}" "$label" $((SECONDS - t0)) "$C_RST" >&2
      i=$((i + 1))
      sleep 0.1
    done
    printf '\r\e[K' >&2
  fi
  while :; do
    wait "$pid" 2>/dev/null
    rc=$?
    kill -0 "$pid" 2>/dev/null || break
    if [[ -n $INTERRUPTED ]]; then
      tries=$((tries + 1))
      if ((tries > 3)); then kill_tree "$pid" KILL; else kill_tree "$pid" TERM; fi
      sleep 0.2
    fi
  done
  CHILD_PID=
  return $rc
}

kill_tree() {
  local pid=$1 sig=${2:-TERM} kids k
  kids=$(ps -A -o pid= -o ppid= 2>/dev/null | while read -r k parent; do [[ $parent == "$pid" ]] && printf '%s ' "$k"; done)
  kill -"$sig" "$pid" 2>/dev/null
  for k in $kids; do kill_tree "$k" "$sig"; done
}

# ---------------------------------------------------------------- time

# Sets NOW_MS (epoch milliseconds) and NOW_ISO (UTC).
now() {
  if [[ -n ${EPOCHREALTIME:-} ]]; then
    local s=${EPOCHREALTIME%%[.,]*} f=${EPOCHREALTIME#*[.,]}
    f=${f}000
    f=${f:0:3}
    NOW_MS=$((s * 1000 + 10#$f))
    TZ=UTC printf -v NOW_ISO '%(%Y-%m-%dT%H:%M:%S)T' "$s"
    NOW_ISO+=".${f}Z"
  else
    local d
    d=$(date -u '+%s %Y-%m-%dT%H:%M:%SZ')
    NOW_MS=$((${d%% *} * 1000))
    NOW_ISO=${d#* }
  fi
}

new_id() {
  printf -v REPLY '%04x%04x-%04x-4%03x-%04x-%04x%04x%04x' \
    $RANDOM $RANDOM $RANDOM $((RANDOM & 4095)) $(((RANDOM & 16383) | 32768)) $RANDOM $RANDOM $RANDOM
}

# ---------------------------------------------------------------- json
#
# json_esc STR       REPLY = STR as a JSON string literal
# json_parse TEXT    flattens TEXT into _JK (paths like choices.0.message.content),
#                    _JV (decoded strings, scalars, or raw JSON for containers) and
#                    _JY (s n b z o a). Returns 1 on invalid JSON.
# jget PATH          REPLY = value at PATH, JTYPE = its type. Returns 1 when absent.

json_setup() {
  J_Q='"' J_QE='\"' J_B='\' J_BE='\\' J_NL=$'\n' J_NLE='\n' J_TAB=$'\t' J_TABE='\t' J_CR=$'\r' J_CRE='\r'
  J_DELIM='{}[]:,'
  J_NUM='^-?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)?$'
  J_CTL=() J_CTLE=() J_CTLS=
  U_CONT= U_LEAD2= U_LEAD3= U_LEAD4=
  local i o c
  for ((i = 128; i < 248; i++)); do
    printf -v o '%03o' "$i"
    printf -v c "\\$o"
    if ((i < 192)); then U_CONT+=$c; elif ((i < 224)); then U_LEAD2+=$c; elif ((i < 240)); then U_LEAD3+=$c; else U_LEAD4+=$c; fi
  done
  for ((i = 1; i < 32; i++)); do
    case $i in 9 | 10 | 13) continue ;; esac
    printf -v o '%03o' "$i"
    printf -v c "\\$o"
    J_CTL+=("$c")
    J_CTLS+=$c
    printf -v o '\\u%04x' "$i"
    J_CTLE+=("$o")
  done
}

json_esc() {
  local s=$1
  case $s in
    *[\"\\]* | *["$J_NL$J_TAB$J_CR"]* | *["$J_CTLS"]*) ;;
    *) REPLY="\"$s\"" && return 0 ;;
  esac
  if [[ -n $OLD_BASH ]] && ((${#s} > 512)); then
    # bash < 4.3 substitutes in O(length x matches); escaping is per byte, so chunk it.
    local -a buf
    local IFS= o n=${#s} k=0
    for ((o = 0; o < n; o += 512)); do
      _jesc "${s:o:512}"
      buf[k]=$REPLY
      k=$((k + 1))
    done
    REPLY="\"${buf[*]}\""
  else
    _jesc "$s"
    REPLY="\"$REPLY\""
  fi
}

_jesc() {
  local s=$1 i
  s=${s//"$J_B"/"$J_BE"}
  s=${s//"$J_Q"/"$J_QE"}
  s=${s//"$J_NL"/"$J_NLE"}
  s=${s//"$J_TAB"/"$J_TABE"}
  s=${s//"$J_CR"/"$J_CRE"}
  case $s in
    *["$J_CTLS"]*) for i in "${!J_CTL[@]}"; do s=${s//"${J_CTL[i]}"/"${J_CTLE[i]}"}; done ;;
  esac
  REPLY=$s
}

json_parse() {
  _JK=() _JV=() _JY=() _JC=0
  _TK=() _TV=() _TR=() _TN=0 _TI=0
  _jtokenize "$1" || return 1
  ((_TN > 0)) || return 1
  _jvalue "" || return 1
  ((_TI == _TN))
}

jget() {
  local i
  for ((i = 0; i < _JC; i++)); do
    if [[ ${_JK[i]} == "$1" ]]; then
      REPLY=${_JV[i]} JTYPE=${_JY[i]}
      return 0
    fi
  done
  REPLY= JTYPE=
  return 1
}

# Split on '"' once, then glue pieces back together where a quote was escaped.
# Linear in the input, which matters because bash string slicing copies.
_jtokenize() {
  local -a P seg
  local IFS='"' n i k raw
  read -r -d '' -a P <<<"$1"
  IFS=$' \t\n'
  n=${#P[@]}
  ((n)) || return 1
  P[n - 1]=${P[n - 1]%"$J_NL"}
  i=0
  while :; do
    _jstruct "${P[i]}" || return 1
    i=$((i + 1))
    ((i < n)) || break
    raw=${P[i]}
    if _jodd "$raw"; then
      # Appending to a growing string copies it each time; join once instead.
      seg=("$raw")
      k=1
      while _jodd "${seg[k - 1]}"; do
        i=$((i + 1))
        ((i < n)) || return 1
        seg[k]=${P[i]}
        k=$((k + 1))
      done
      IFS='"'
      raw="${seg[*]}"
      IFS=$' \t\n'
    fi
    i=$((i + 1))
    ((i < n)) || return 1
    _jdecode "$raw"
    _TK[_TN]=s _TV[_TN]=$REPLY _TR[_TN]=$raw
    _TN=$((_TN + 1))
  done
}

_jodd() {
  local s=$1 k=0
  while [[ ${s: -1} == "$J_B" ]]; do
    s=${s%?}
    k=$((k + 1))
  done
  ((k % 2))
}

_jstruct() {
  local p=$1 w
  while [[ -n $p ]]; do
    case $p in
      [[:space:]]*)
        w=${p%%[![:space:]]*}
        p=${p:${#w}}
        ;;
      ["$J_DELIM"]*)
        _TK[_TN]=${p:0:1}
        _TN=$((_TN + 1))
        p=${p:1}
        ;;
      *)
        w=${p%%["$J_DELIM"[:space:]]*}
        _TK[_TN]=v _TV[_TN]=$w
        _TN=$((_TN + 1))
        p=${p:${#w}}
        ;;
    esac
  done
}

_jdecode() {
  case $1 in
    *\\*) ;;
    *) REPLY=$1 && return 0 ;;
  esac
  local s=$1
  if [[ -z $OLD_BASH && $s != *'\u'* ]]; then
    # Raw JSON strings cannot hold control characters, so \001 is a safe stand-in for "\\".
    s=${s//"$J_BE"/$'\001'}
    s=${s//"$J_NLE"/"$J_NL"}
    s=${s//"$J_QE"/"$J_Q"}
    s=${s//"$J_TABE"/"$J_TAB"}
    s=${s//'\/'/'/'}
    s=${s//"$J_CRE"/"$J_CR"}
    s=${s//'\b'/$'\b'}
    s=${s//'\f'/$'\f'}
    REPLY=${s//$'\001'/"$J_B"}
    return 0
  fi
  local -a parts buf
  local IFS='\' p c i n k cp lo
  read -r -d '' -a parts <<<"$s"
  IFS=$' \t\n'
  n=${#parts[@]}
  parts[n - 1]=${parts[n - 1]%"$J_NL"}
  buf[0]=${parts[0]}
  k=1
  i=1
  while ((i < n)); do
    p=${parts[i]}
    if [[ -z $p ]]; then
      # "\\": an escaped backslash; the piece after it is plain text.
      i=$((i + 1))
      buf[k]=$J_B${parts[i]}
      k=$((k + 1))
      i=$((i + 1))
      continue
    fi
    c=${p:0:1}
    p=${p:1}
    case $c in
      n) c=$J_NL ;;
      t) c=$J_TAB ;;
      r) c=$J_CR ;;
      b) c=$'\b' ;;
      f) c=$'\f' ;;
      u)
        cp=$((16#${p:0:4}))
        p=${p:4}
        if ((cp >= 0xD800 && cp < 0xDC00)) && [[ -z $p ]] && ((i + 1 < n)) && [[ ${parts[i + 1]} == u[dD][c-fC-F]??* ]]; then
          i=$((i + 1))
          lo=$((16#${parts[i]:1:4}))
          p=${parts[i]:5}
          cp=$((0x10000 + ((cp - 0xD800) << 10) + (lo - 0xDC00)))
        fi
        _jutf8 "$cp"
        c=$REPLY
        ;;
    esac
    buf[k]=$c$p
    k=$((k + 1))
    i=$((i + 1))
  done
  IFS=
  REPLY="${buf[*]}"
  IFS=$' \t\n'
}

_jutf8() {
  local cp=$1 f
  if ((cp < 0x80)); then
    printf -v f '\\%03o' "$cp"
  elif ((cp < 0x800)); then
    printf -v f '\\%03o\\%03o' $((0xC0 | cp >> 6)) $((0x80 | (cp & 0x3F)))
  elif ((cp < 0x10000)); then
    printf -v f '\\%03o\\%03o\\%03o' $((0xE0 | cp >> 12)) $((0x80 | ((cp >> 6) & 0x3F))) $((0x80 | (cp & 0x3F)))
  else
    printf -v f '\\%03o\\%03o\\%03o\\%03o' $((0xF0 | cp >> 18)) $((0x80 | ((cp >> 12) & 0x3F))) \
      $((0x80 | ((cp >> 6) & 0x3F))) $((0x80 | (cp & 0x3F)))
  fi
  printf -v REPLY "$f"
}

_jput() {
  _JK[_JC]=$1 _JY[_JC]=$2 _JV[_JC]=$3
  _JC=$((_JC + 1))
}

_jvalue() {
  local w t
  case ${_TK[_TI]} in
    s)
      _jput "$1" s "${_TV[_TI]}"
      _JRAW="\"${_TR[_TI]}\""
      _TI=$((_TI + 1))
      ;;
    v)
      w=${_TV[_TI]}
      case $w in
        true | false) t=b ;;
        null) t=z ;;
        *) [[ $w =~ $J_NUM ]] && t=n || return 1 ;;
      esac
      _jput "$1" "$t" "$w"
      _JRAW=$w
      _TI=$((_TI + 1))
      ;;
    '{') _jobject "$1" ;;
    '[') _jarray "$1" ;;
    *) return 1 ;;
  esac
}

_jobject() {
  local path=$1 raw='{' key
  _TI=$((_TI + 1))
  if [[ ${_TK[_TI]} == '}' ]]; then
    _TI=$((_TI + 1))
    _jput "$path" o '{}'
    _JRAW='{}'
    return 0
  fi
  while :; do
    [[ ${_TK[_TI]} == s ]] || return 1
    key=${_TV[_TI]}
    raw+="\"${_TR[_TI]}\":"
    _TI=$((_TI + 1))
    [[ ${_TK[_TI]} == : ]] || return 1
    _TI=$((_TI + 1))
    _jvalue "${path:+$path.}$key" || return 1
    raw+=$_JRAW
    case ${_TK[_TI]} in
      ,) raw+=, && _TI=$((_TI + 1)) ;;
      '}') _TI=$((_TI + 1)) && break ;;
      *) return 1 ;;
    esac
  done
  raw+='}'
  _jput "$path" o "$raw"
  _JRAW=$raw
}

_jarray() {
  local path=$1 raw='[' idx=0
  _TI=$((_TI + 1))
  if [[ ${_TK[_TI]} == ']' ]]; then
    _TI=$((_TI + 1))
    _jput "$path" a '[]'
    _JRAW='[]'
    return 0
  fi
  while :; do
    _jvalue "${path:+$path.}$idx" || return 1
    raw+=$_JRAW
    idx=$((idx + 1))
    case ${_TK[_TI]} in
      ,) raw+=, && _TI=$((_TI + 1)) ;;
      ']') _TI=$((_TI + 1)) && break ;;
      *) return 1 ;;
    esac
  done
  raw+=']'
  _jput "$path" a "$raw"
  _JRAW=$raw
}

# ---------------------------------------------------------------- tracing (Neatlogs HTTP ingest)
#
# One nested JSON tree per entry-point invocation, POSTed once when it finishes.
# Open spans live on a stack so an interrupted run can still be closed and sent.

trace_init() {
  TRACE= SPAN_D=-1 TRACE_ROOT=
  SPAN_HEAD=() SPAN_KIDS=() SPAN_T0=()
  [[ -n ${NEATLOGS_API_KEY:-} ]] || return 0
  case $NEATLOGS_API_KEY in
    nlw_*)
      if [[ -z ${NEATLOGS_PROJECT_NAME:-} ]]; then
        warn "NEATLOGS_API_KEY is a write key (nlw_); set NEATLOGS_PROJECT_NAME to the dashboard project name. Tracing is off."
        return 0
      fi
      ;;
  esac
  TRACE=1
}

span_open() { # name kind [extra fields]
  [[ -n $TRACE ]] || return 0
  now
  json_esc "$1"
  SPAN_D=$((SPAN_D + 1))
  SPAN_HEAD[SPAN_D]="\"name\":$REPLY,\"kind\":\"$2\",\"start\":\"$NOW_ISO\"${3:+,$3}"
  SPAN_T0[SPAN_D]=$NOW_MS
  SPAN_KIDS[SPAN_D]=
}

span_close() { # status [output json] [extra fields] [error message]
  [[ -n $TRACE ]] && ((SPAN_D >= 0)) || return 0
  local d=$SPAN_D s
  now
  s="{${SPAN_HEAD[d]},\"end\":\"$NOW_ISO\",\"duration_ms\":$((NOW_MS - SPAN_T0[d])),\"status\":\"$1\""
  [[ -n ${2:-} ]] && s+=",\"output\":$2"
  [[ -n ${3:-} ]] && s+=",$3"
  if [[ -n ${4:-} ]]; then
    json_esc "$4"
    s+=",\"error\":$REPLY"
  fi
  [[ -n ${SPAN_KIDS[d]} ]] && s+=",\"children\":[${SPAN_KIDS[d]}]"
  s+='}'
  unset "SPAN_HEAD[$d]" "SPAN_KIDS[$d]" "SPAN_T0[$d]"
  SPAN_D=$((d - 1))
  if ((SPAN_D >= 0)); then
    SPAN_KIDS[SPAN_D]+=${SPAN_KIDS[SPAN_D]:+,}$s
  else
    TRACE_ROOT=$s
  fi
}

span_unwind() { # depth status error: close every span above depth
  [[ -n $TRACE ]] || return 0
  while ((SPAN_D > $1)); do span_close "$2" "" "" "$3"; done
}

trace_begin() { # workflow input
  [[ -n $TRACE ]] || return 0
  SPAN_D=-1 TRACE_ROOT=
  local extra attrs=
  json_esc "$2"
  extra="\"input\":$REPLY"
  case $NEATLOGS_API_KEY in
    nlw_*) json_esc "$NEATLOGS_PROJECT_NAME" && extra+=",\"project\":$REPLY" ;;
  esac
  if [[ -n ${SESSION_ID:-} ]]; then
    json_esc "$SESSION_ID"
    attrs+="\"neatlogs.session.id\":$REPLY"
  fi
  if [[ -n ${NEATLOGS_END_USER_ID:-} ]]; then
    json_esc "$NEATLOGS_END_USER_ID"
    attrs+="${attrs:+,}\"neatlogs.end_user.id\":$REPLY"
  fi
  [[ -n $attrs ]] && extra+=",\"attributes\":{$attrs}"
  span_open "$1" WORKFLOW "$extra"
}

trace_end() { # status output [error]
  [[ -n $TRACE ]] && ((SPAN_D >= 0)) || return 0
  local out=
  span_unwind 0 ERROR "${3:-aborted}"
  if [[ -n $2 ]]; then
    json_esc "$2"
    out=$REPLY
  fi
  span_close "$1" "$out" "" "${3:-}"
  trace_send
}

trace_send() {
  [[ -n $TRACE && -n $TRACE_ROOT ]] || return 0
  local f=$SHY_TMP/trace.json res=$SHY_TMP/trace.res code body
  printf '%s' "$TRACE_ROOT" >"$f"
  TRACE_ROOT=
  code=$(curl -sS --max-time 20 -o "$res" -w '%{http_code}' -X POST "$NEATLOGS_INGEST_URL/v1/trace" \
    -H 'Content-Type: application/json' -K - --data-binary @"$f" 2>"$res.err" <<EOF
header = "x-api-key: $NEATLOGS_API_KEY"
EOF
  )
  body=
  [[ -f $res ]] && IFS= read -r -d '' body <"$res"
  if [[ $code == 2* ]]; then
    json_parse "$body" && jget trace_id && note "· neatlogs trace $REPLY"
  else
    [[ -z $body && -f $res.err ]] && IFS= read -r -d '' body <"$res.err"
    oneline "$body" 300
    warn "neatlogs ingest failed (HTTP ${code:-none}): $REPLY"
  fi
  rm -f "$f" "$res" "$res.err"
  return 0
}

# ---------------------------------------------------------------- tools

tools_setup() {
  local read edit write bash fetch sub
  read='{"type":"function","function":{"name":"read","description":"Read the contents of a text file. Output is truncated to 2000 lines or 50KB, whichever comes first; use offset and limit to page through large files.","parameters":{"type":"object","properties":{"path":{"type":"string","description":"Path to the file (relative or absolute)"},"offset":{"type":"integer","description":"Line number to start reading from (1-indexed)"},"limit":{"type":"integer","description":"Maximum number of lines to read"}},"required":["path"]}}}'
  edit='{"type":"function","function":{"name":"edit","description":"Edit a file by replacing exact text. oldText must match exactly one location in the file, including whitespace and indentation. Use this for precise, surgical edits.","parameters":{"type":"object","properties":{"path":{"type":"string","description":"Path to the file (relative or absolute)"},"oldText":{"type":"string","description":"Exact text to find and replace"},"newText":{"type":"string","description":"Text to replace it with"}},"required":["path","oldText","newText"]}}}'
  write='{"type":"function","function":{"name":"write","description":"Write content to a file. Creates the file if it does not exist and overwrites it if it does. Parent directories are created automatically.","parameters":{"type":"object","properties":{"path":{"type":"string","description":"Path to the file (relative or absolute)"},"content":{"type":"string","description":"Full content to write"}},"required":["path","content"]}}}'
  bash='{"type":"function","function":{"name":"bash","description":"Run a bash command in the current working directory. Returns stdout and stderr combined, truncated to the last 50KB. Commands do not share state; stdin is empty.","parameters":{"type":"object","properties":{"command":{"type":"string","description":"Bash command to run"},"timeout":{"type":"integer","description":"Timeout in seconds (default 300)"}},"required":["command"]}}}'
  fetch='{"type":"function","function":{"name":"webfetch","description":"Fetch a URL over HTTP(S). HTML pages are converted to plain text; set raw to true to get the original body. Output is truncated to 50KB.","parameters":{"type":"object","properties":{"url":{"type":"string","description":"http:// or https:// URL"},"raw":{"type":"boolean","description":"Return the body without HTML-to-text conversion"}},"required":["url"]}}}'
  sub='{"type":"function","function":{"name":"subagent","description":"Hand a self-contained task to a subagent that starts with a fresh context and has the same tools (read, edit, write, bash, webfetch, subagent). Returns the final answer of the subagent. Use it for broad exploration or independent sub-tasks so your own context stays small. The subagent cannot see this conversation, so describe the task completely.","parameters":{"type":"object","properties":{"task":{"type":"string","description":"Complete description of the task and what to report back"}},"required":["task"]}}}'
  TOOLS_BASE="$read,$edit,$write,$bash,$fetch"
  TOOLS_ALL="$TOOLS_BASE,$sub"
}

# TOOL_FAILED holds a short error message for the trace; the model gets TOOL_OUT.
tool_fail() {
  TOOL_OUT=$1 TOOL_FAILED=$1
}

expand_path() {
  case $1 in
    "~") REPLY=$HOME ;;
    "~/"*) REPLY=$HOME/${1:2} ;;
    *) REPLY=$1 ;;
  esac
}

# Accepts integers, also as "12" or 12.0; anything else gives $2.
int_arg() {
  local v=${1%.0}
  case $v in
    '' | *[!0-9]*) REPLY=$2 ;;
    *) REPLY=$((10#$v)) ;;
  esac
}

tool_read() {
  local path offset limit line out= lineno=0 shown=0 bytes=0 more=
  jget path
  expand_path "$REPLY"
  path=$REPLY
  jget offset
  int_arg "$REPLY" 1
  offset=$REPLY
  jget limit
  int_arg "$REPLY" "$MAX_LINES"
  limit=$REPLY
  ((offset < 1)) && offset=1
  ((limit < 1 || limit > MAX_LINES)) && limit=$MAX_LINES
  [[ -n $path ]] || { tool_fail "path is required"; return; }
  [[ -d $path ]] && { tool_fail "$path is a directory; use bash (ls) to list it"; return; }
  [[ -e $path ]] || { tool_fail "File not found: $path"; return; }
  [[ -r $path ]] || { tool_fail "Permission denied: $path"; return; }
  while IFS= read -r line || [[ -n $line ]]; do
    lineno=$((lineno + 1))
    ((lineno < offset)) && continue
    if ((shown >= limit || bytes + ${#line} + 1 > MAX_BYTES)); then
      more=1
      break
    fi
    out+=$line$'\n'
    shown=$((shown + 1))
    bytes=$((bytes + ${#line} + 1))
  done <"$path"
  if ((shown == 0)); then
    if [[ -n $more ]]; then
      utf8_head "${line:0:MAX_BYTES}"
      TOOL_OUT="$REPLY"$'\n\n'"[Line $lineno is longer than 50KB and was cut. Use bash (e.g. head -c, cut) to read it in pieces.]"
      TOOL_INFO="read 1 partial line"
      return
    fi
    if ((lineno == 0)); then
      TOOL_OUT="(empty file)"
    else
      TOOL_OUT="Offset $offset is past the end of the file ($lineno lines)."
    fi
    TOOL_INFO=$TOOL_OUT
    return
  fi
  TOOL_INFO="read $shown lines"
  if [[ -n $more ]]; then
    out+=$'\n'"[Showing lines $offset-$((offset + shown - 1)). More lines follow; use offset=$((offset + shown)) to continue.]"
  fi
  TOOL_OUT=$out
}

tool_write() {
  local path content dir
  jget path
  expand_path "$REPLY"
  path=$REPLY
  jget content || { tool_fail "content is required"; return; }
  content=$REPLY
  [[ -n $path ]] || { tool_fail "path is required"; return; }
  if [[ $path == */* ]]; then
    dir=${path%/*}
    [[ -z $dir || -d $dir ]] || mkdir -p "$dir" 2>/dev/null || { tool_fail "Could not create directory $dir"; return; }
  fi
  printf '%s' "$content" >"$path" 2>/dev/null || { tool_fail "Could not write $path"; return; }
  TOOL_OUT="Wrote ${#content} bytes to $path"
}

tool_edit() {
  local path old new content before after lo hi mid
  jget path
  expand_path "$REPLY"
  path=$REPLY
  jget oldText || jget old_string
  old=$REPLY
  jget newText || jget new_string
  new=$REPLY
  [[ -n $path ]] || { tool_fail "path is required"; return; }
  [[ -f $path ]] || { tool_fail "File not found: $path"; return; }
  [[ -n $old ]] || { tool_fail "oldText must not be empty"; return; }
  content=
  IFS= read -r -d '' content <"$path"
  case $content in
    *"$old"*) ;;
    *)
      tool_fail "Could not find oldText in $path. It must match the file exactly, including whitespace and indentation. Read the file again before retrying."
      return
      ;;
  esac
  # Shortest prefix that contains oldText, by bisection: substring tests stay linear
  # on bash 3.2, where ${content%%"$old"*} is quadratic.
  lo=${#old} hi=${#content}
  while ((lo < hi)); do
    mid=$(((lo + hi) / 2))
    case ${content:0:mid} in
      *"$old"*) hi=$mid ;;
      *) lo=$((mid + 1)) ;;
    esac
  done
  before=${content:0:lo-${#old}}
  after=${content:lo}
  case $after in
    *"$old"*)
      tool_fail "oldText matches more than one location in $path. Include more surrounding lines so it matches exactly once."
      return
      ;;
  esac
  printf '%s' "$before$new$after" >"$path" 2>/dev/null || { tool_fail "Could not write $path"; return; }
  TOOL_OUT="Edited $path: replaced ${#old} bytes with ${#new} bytes."
}

tool_bash() {
  local cmd timeout out rc mark pid wd total body
  jget command
  cmd=$REPLY
  jget timeout
  int_arg "$REPLY" "$BASH_TIMEOUT"
  timeout=$REPLY
  ((timeout < 1)) && timeout=$BASH_TIMEOUT
  [[ -n $cmd ]] || { tool_fail "command is required"; return; }
  out=$SHY_TMP/bash.$RANDOM$RANDOM.out
  mark=$out.timeout
  (
    locale_user
    export GIT_TERMINAL_PROMPT=0 GIT_PAGER=cat PAGER=cat
    exec "${BASH:-bash}" -c "$cmd"
  ) >"$out" 2>&1 </dev/null &
  pid=$!
  (
    sleep "$timeout"
    : >"$mark"
    kill_tree "$pid" TERM
    sleep 3
    kill_tree "$pid" KILL
  ) >/dev/null 2>&1 &
  wd=$!
  disown "$wd" 2>/dev/null
  spin_wait "$pid" "running"
  rc=$?
  kill_tree "$wd" KILL
  body=
  IFS= read -r -d '' body <"$out"
  total=${#body}
  if ((total > MAX_BYTES)); then
    utf8_tail "${body: -MAX_BYTES}"
    body="[Output truncated: showing the last 50KB of $total bytes. Full output: $out]"$'\n'"$REPLY"
  else
    rm -f "$out"
  fi
  body=${body%$'\n'}
  [[ -z $body ]] && body="(no output)"
  if [[ -f $mark ]]; then
    rm -f "$mark"
    TOOL_FAILED="Timed out after ${timeout}s"
  elif [[ -n $INTERRUPTED ]]; then
    TOOL_FAILED="Interrupted by user"
  elif ((rc != 0)); then
    TOOL_FAILED="Exit code: $rc"
  fi
  [[ -n $TOOL_FAILED ]] && body+=$'\n'"[$TOOL_FAILED]"
  TOOL_OUT=$body
}

tool_webfetch() {
  local url raw f code ctype size rc body
  jget url
  url=$REPLY
  jget raw
  raw=$REPLY
  case $url in
    http://* | https://*) ;;
    *) tool_fail "url must start with http:// or https://" && return ;;
  esac
  f=$SHY_TMP/fetch.$RANDOM$RANDOM
  curl -sSL --max-time 30 --max-filesize 10000000 -A "Mozilla/5.0 (compatible; shy/$SHY_VERSION)" \
    -o "$f" -w '%{http_code} %{content_type}' "$url" >"$f.meta" 2>"$f.err" &
  spin_wait $! "fetching"
  rc=$?
  if ((rc != 0)); then
    body=
    IFS= read -r -d '' body <"$f.err"
    rm -f "$f" "$f.meta" "$f.err"
    tool_fail "Fetch failed (curl exit $rc): ${body:-no details}"
    return
  fi
  read -r code ctype <"$f.meta"
  rm -f "$f.meta" "$f.err"
  body=
  IFS= read -r -d '' body <"$f"
  size=${#body}
  case $ctype in
    *html*)
      if [[ $raw == true ]]; then rm -f "$f"; else html_to_text "$body" && body=$REPLY; rm -f "$f"; fi
      ;;
    '' | text/* | *json* | *xml* | *javascript* | *yaml* | *csv*) rm -f "$f" ;;
    *)
      body="Binary content ($ctype, $size bytes) saved to $f. Use bash to inspect it."
      ;;
  esac
  if ((${#body} > MAX_BYTES)); then
    utf8_head "${body:0:MAX_BYTES}"
    body="$REPLY"$'\n\n'"[Truncated at 50KB of text from a $size byte response]"
  fi
  TOOL_OUT="HTTP $code ${ctype:-unknown type}"$'\n\n'"$body"
  TOOL_INFO="HTTP $code · ${ctype%%;*} · ${#body} bytes"
  [[ $code == [23]* ]] || TOOL_FAILED="HTTP $code"
}

# Rough HTML to text: drop script/style, break lines at block tags, collapse spaces.
html_to_text() {
  local -a parts buf
  local IFS='<' part tag text name skip= i n k=0 len=0 bol=1 pfx=
  read -r -d '' -a parts <<<"$1"
  IFS=$' \t\n'
  n=${#parts[@]}
  shopt -s nocasematch
  for ((i = 1; i < n; i++)); do
    part=${parts[i]}
    tag=${part%%>*}
    if ((${#tag} == ${#part})); then text=; else text=${part:${#tag}+1}; fi
    if [[ -n $skip ]]; then
      [[ $tag == "/$skip"* ]] && skip=
      continue
    fi
    name=${tag#/}
    name=${name%%[[:space:]/]*}
    case $name in
      script | style | noscript | svg | template)
        [[ $tag == /* || $tag == */ ]] || skip=$name
        continue
        ;;
      p | div | br | li | h[1-6] | tr | ul | ol | table | section | article | header | footer | nav | main | aside | pre | blockquote | title | dt | dd | hr | form)
        if ((!bol)); then
          buf[k]=$'\n'
          k=$((k + 1))
          bol=1
          pfx=
        fi
        [[ $name == li && $tag != /* ]] && pfx="- "
        ;;
    esac
    [[ -n $text ]] || continue
    if ((${#text} < 8192)); then
      text=${text//[[:space:]]/ }
      while [[ $text == *"  "* ]]; do text=${text//"  "/" "}; done
    fi
    text=${text# }
    text=${text% }
    [[ -n $text ]] || continue
    case $text in
      *'&'*)
        text=${text//'&nbsp;'/ }
        text=${text//'&lt;'/<}
        text=${text//'&gt;'/>}
        text=${text//'&quot;'/\"}
        text=${text//'&#39;'/\'}
        text=${text//'&#x27;'/\'}
        text=${text//'&apos;'/\'}
        text=${text//'&amp;'/&}
        ;;
    esac
    if ((bol)); then text=$pfx$text; else text=" $text"; fi
    buf[k]=$text
    k=$((k + 1))
    len=$((len + ${#text}))
    bol=0
    pfx=
    ((len > MAX_BYTES + 1024)) && break
  done
  shopt -u nocasematch
  IFS=
  REPLY="${buf[*]}"
  IFS=$' \t\n'
}

tool_subagent() {
  local depth=$1 task rc
  local -a msgs
  jget task
  task=$REPLY
  [[ -n $task ]] || { tool_fail "task is required"; return; }
  system_prompt sub $((depth + 1))
  json_esc "$REPLY"
  msgs=("{\"role\":\"system\",\"content\":$REPLY}")
  json_esc "$task"
  msgs+=("{\"role\":\"user\",\"content\":$REPLY}")
  agent_run $((depth + 1)) sub
  rc=$?
  TOOL_FAILED= TOOL_INFO=
  if ((rc == 0)); then
    TOOL_OUT=${AGENT_OUT:-"(the subagent returned no text)"}
  elif ((rc == 130)); then
    tool_fail "Subagent interrupted by user."
  else
    tool_fail "Subagent failed: $AGENT_ERR"
  fi
}

run_tool() { # name args depth
  local name=$1 args=$2 depth=$3 input summary= ok=
  TOOL_OUT= TOOL_FAILED= TOOL_INFO=
  [[ -n $args ]] || args='{}'
  if json_parse "$args" && [[ ${_JY[_JC - 1]} == o ]]; then
    ok=1
    input=$args
  else
    json_esc "$args"
    input=$REPLY
  fi
  if [[ -n $ok ]]; then
    case $name in
      read | write | edit) jget path ;;
      bash) jget command ;;
      webfetch) jget url ;;
      subagent) jget task ;;
      *) REPLY= ;;
    esac
    oneline "$REPLY" 110
    summary=$REPLY
  fi
  printf '%s%s●%s %s%s%s %s%s%s\n' "$UI_PFX" "$C_GRN" "$C_RST" "$C_BOLD" "$name" "$C_RST" "$C_DIM" "$summary" "$C_RST" >&2
  json_esc "$name"
  span_open "$name" TOOL "\"tool_name\":$REPLY,\"input\":$input"
  if [[ -z $ok ]]; then
    tool_fail "Arguments for $name are not a valid JSON object: $args"
  else
    case $name in
      read) tool_read ;;
      write) tool_write ;;
      edit) tool_edit ;;
      bash) tool_bash ;;
      webfetch) tool_webfetch ;;
      subagent)
        if ((depth < MAX_DEPTH)); then tool_subagent "$depth"; else tool_fail "Subagents cannot nest deeper than $MAX_DEPTH levels."; fi
        ;;
      *) tool_fail "Unknown tool: $name" ;;
    esac
  fi
  if [[ $name == bash ]]; then
    show_tail "$TOOL_OUT" 4 "$UI_PFX  ⎿ "
  elif [[ -n $TOOL_FAILED ]]; then
    oneline "$TOOL_OUT" 200
    printf '%s  ⎿ %s%s%s\n' "$UI_PFX" "$C_RED" "$REPLY" "$C_RST" >&2
  elif [[ -n $TOOL_INFO ]]; then
    note "  ⎿ $TOOL_INFO"
  else
    show_lines "$TOOL_OUT" 3 "$UI_PFX  ⎿ "
  fi
  [[ -n $TRACE ]] || return 0
  json_esc "$TOOL_OUT"
  if [[ -n $TOOL_FAILED ]]; then
    input=$REPLY
    oneline "$TOOL_FAILED" 500
    span_close ERROR "$input" "" "$REPLY"
  else
    span_close OK "$REPLY"
  fi
}

# ---------------------------------------------------------------- model

system_prompt() { # main|sub depth
  local p f ctx=
  p="You are shy, an expert coding assistant running in the terminal of the user. You help with coding tasks by reading files, running commands, editing code and writing new files.

Available tools:
- read: Read file contents
- bash: Run bash commands (ls, find, grep, git, tests, builds)
- edit: Make surgical edits to files (replace exact text)
- write: Create or overwrite files
- webfetch: Fetch a URL and get its content as text"
  (($2 < MAX_DEPTH)) && p+="
- subagent: Hand a self-contained task to a subagent with a fresh context"
  p+="

Guidelines:
- Use bash for file discovery and search (ls, find, grep, rg).
- Read a file before editing it. oldText in edit must match the file exactly, including whitespace.
- Use write only for new files or complete rewrites.
- Be concise. When you change files, say which ones."
  (($2 < MAX_DEPTH)) && p+="
- Use subagent for broad exploration or independent work so your own context stays small."
  if [[ $1 == sub ]]; then
    p+="

You are running as a subagent of another shy agent. Finish the task on your own; you cannot ask questions. Your final message is handed back verbatim to the parent agent, which cannot see your tool calls, so make it complete and self-contained."
  fi
  for f in AGENTS.md CLAUDE.md; do
    if [[ -f $f && -r $f ]]; then
      IFS= read -r -d '' ctx <"$f"
      utf8_head "${ctx:0:20000}"
      p+="

# Project instructions ($f)

$REPLY"
      break
    fi
  done
  now
  p+="

Current date: ${NOW_ISO%%T*}
Current working directory: $PWD
Operating system: ${OS_NAME:-unknown}"
  REPLY=$p
}

# Decimal string (e.g. "0.00123") added to U_MICROS in millionths.
add_cost() {
  local c=$1 i f=
  case $c in '' | *[!0-9.]*) return 0 ;; esac
  i=${c%%.*}
  [[ $c == *.* ]] && f=${c#*.}
  f=${f}000000
  f=${f:0:6}
  U_MICROS=$((U_MICROS + 10#${i:-0} * 1000000 + 10#$f))
}

# Messages for the LLM span input, oldest dropped past TRACE_INPUT_MAX bytes.
trace_messages() {
  local IFS=, n=${#msgs[@]} i total=0
  for ((i = n - 1; i >= 1; i--)); do
    total=$((total + ${#msgs[i]}))
    ((total > TRACE_INPUT_MAX)) && break
  done
  if ((i < 1)); then
    REPLY="[${msgs[*]}]"
  else
    REPLY="[${msgs[0]},{\"role\":\"system\",\"content\":\"[$i earlier messages omitted from this trace]\"},${msgs[*]:i+1}]"
  fi
}

# POST the conversation. Sets L_* globals. Returns 0, 1 (error) or 130 (interrupted).
llm_request() { # tools
  local req=$SHY_TMP/req.json out=$SHY_TMP/resp.json body joined attempt=0 rc code err= wait_s resp i n
  local content ctype tc pt=0 ct=0 tt=0 cached= reasoning_toks= model provider extra attrs msg
  L_CONTENT= L_REASONING= L_FINISH= L_ERR= L_MSG= L_TC_N=0
  L_TC_ID=() L_TC_NAME=() L_TC_ARGS=()
  IFS=,
  joined="${msgs[*]}"
  IFS=$' \t\n'
  json_esc "$MODEL"
  body="{\"model\":$REPLY,\"messages\":[$joined],\"tools\":[$1]}"
  printf '%s' "$body" >"$req"
  if [[ -n $TRACE ]]; then
    trace_messages
    span_open model-call LLM "\"input\":$REPLY"
  fi

  while :; do
    attempt=$((attempt + 1))
    rm -f "$out"
    curl -sS --max-time 900 -o "$out" -w '%{http_code}' -X POST "$OPENROUTER_URL/chat/completions" \
      -K - --data-binary @"$req" >"$out.code" 2>"$out.err" <<EOF &
header = "Authorization: Bearer $OPENROUTER_API_KEY"
header = "Content-Type: application/json"
header = "X-Title: shy"
EOF
    spin_wait $! "thinking"
    rc=$?
    if [[ -n $INTERRUPTED ]]; then
      L_ERR="interrupted by user"
      span_close ERROR "" "" "$L_ERR"
      return 130
    fi
    code=
    read -r code <"$out.code" 2>/dev/null
    resp=
    [[ -f $out ]] && IFS= read -r -d '' resp <"$out"
    err=
    if ((rc != 0)); then
      IFS= read -r -d '' err <"$out.err"
      err="network error (curl exit $rc): ${err%$'\n'}"
    elif [[ $code != 200 ]]; then
      if json_parse "$resp" && jget error.message; then err="HTTP $code: $REPLY"; else oneline "$resp" 300 && err="HTTP $code: $REPLY"; fi
    fi
    [[ -z $err ]] && break
    case "$rc:$code" in
      [1-9]*:* | 0:408 | 0:429 | 0:500 | 0:502 | 0:503 | 0:504 | 0:529)
        if ((attempt < 4)); then
          wait_s=$((2 ** attempt))
          warn "$err (retrying in ${wait_s}s)"
          sleep "$wait_s"
          [[ -n $INTERRUPTED ]] && { L_ERR="interrupted by user"; span_close ERROR "" "" "$L_ERR"; return 130; }
          continue
        fi
        ;;
    esac
    L_ERR=$err
    span_close ERROR "" "" "$L_ERR"
    return 1
  done

  if ! json_parse "$resp"; then
    oneline "$resp" 300
    L_ERR="could not parse the model response: $REPLY"
    span_close ERROR "" "" "$L_ERR"
    return 1
  fi
  if jget error.message || jget choices.0.error.message; then
    L_ERR="provider error: $REPLY"
    span_close ERROR "" "" "$L_ERR"
    return 1
  fi
  if ! jget choices.0.message; then
    L_ERR="response has no choices"
    span_close ERROR "" "" "$L_ERR"
    return 1
  fi

  jget choices.0.message.content
  content=$REPLY ctype=$JTYPE
  [[ $ctype == s ]] || content=
  if [[ $ctype == a ]]; then
    for ((i = 0; ; i++)); do
      jget "choices.0.message.content.$i" || break
      jget "choices.0.message.content.$i.text" && content+=$REPLY
    done
  fi
  jget choices.0.message.reasoning && [[ $JTYPE == s ]] && L_REASONING=$REPLY
  jget choices.0.finish_reason && L_FINISH=$REPLY
  model=$MODEL
  jget model && [[ -n $REPLY ]] && model=$REPLY
  provider=
  jget provider && provider=$REPLY

  n=0
  tc=
  while jget "choices.0.message.tool_calls.$n"; do
    jget "choices.0.message.tool_calls.$n.id"
    L_TC_ID[n]=${REPLY:-call_${RANDOM}_$n}
    jget "choices.0.message.tool_calls.$n.function.name"
    L_TC_NAME[n]=$REPLY
    jget "choices.0.message.tool_calls.$n.function.arguments"
    L_TC_ARGS[n]=$REPLY
    n=$((n + 1))
  done
  L_TC_N=$n

  jget usage.prompt_tokens && pt=$REPLY
  jget usage.completion_tokens && ct=$REPLY
  jget usage.total_tokens && tt=$REPLY
  jget usage.prompt_tokens_details.cached_tokens && cached=$REPLY
  jget usage.completion_tokens_details.reasoning_tokens && reasoning_toks=$REPLY
  int_arg "$pt" 0 && pt=$REPLY
  int_arg "$ct" 0 && ct=$REPLY
  int_arg "$tt" $((pt + ct)) && tt=$REPLY
  U_IN=$((U_IN + pt)) U_OUT=$((U_OUT + ct)) U_CALLS=$((U_CALLS + 1))
  jget usage.cost && add_cost "$REPLY"

  # Assistant message for the history; reasoning_details is passed back as-is
  # because some providers need it to continue after tool calls.
  if ((n > 0)); then
    for ((i = 0; i < n; i++)); do
      json_esc "${L_TC_ID[i]}"
      tc+="${tc:+,}{\"id\":$REPLY,\"type\":\"function\",\"function\":{\"name\":"
      json_esc "${L_TC_NAME[i]}"
      tc+="$REPLY,\"arguments\":"
      json_esc "${L_TC_ARGS[i]}"
      tc+="$REPLY}}"
    done
  fi
  if [[ -n $content ]]; then
    json_esc "$content"
    msg="{\"role\":\"assistant\",\"content\":$REPLY"
  elif ((n > 0)); then
    msg='{"role":"assistant","content":null'
  else
    msg='{"role":"assistant","content":""'
  fi
  ((n > 0)) && msg+=",\"tool_calls\":[$tc]"
  jget choices.0.message.reasoning_details && [[ $JTYPE == a ]] && msg+=",\"reasoning_details\":$REPLY"
  msg+='}'
  L_CONTENT=$content
  L_MSG=$msg

  if [[ -n $TRACE ]]; then
    json_esc "$model"
    extra="\"model\":$REPLY,\"tokens\":{\"prompt\":$pt,\"completion\":$ct,\"total\":$tt}"
    attrs='"neatlogs.llm.provider":"openrouter"'
    if [[ -n $L_FINISH ]]; then
      json_esc "$L_FINISH"
      attrs+=",\"neatlogs.llm.finish_reason\":$REPLY"
    fi
    int_arg "$reasoning_toks" ''
    [[ -n $REPLY ]] && attrs+=",\"neatlogs.llm.token_count.reasoning\":$REPLY"
    int_arg "$cached" ''
    [[ -n $REPLY ]] && attrs+=",\"neatlogs.llm.token_count.cache_read\":$REPLY"
    json_esc "$SYSTEM_TEXT"
    attrs+=",\"neatlogs.llm.system_prompt\":$REPLY"
    span_close OK "$msg" "$extra,\"attributes\":{$attrs}"
  fi
  return 0
}

# ---------------------------------------------------------------- agent loop

# Runs model/tool turns on the caller's msgs array until the model stops calling tools.
# Sets AGENT_OUT (final text) or AGENT_ERR. Returns 0, 1 or 130.
agent_run() { # depth main|sub
  local saved_pfx=$UI_PFX rc i
  UI_PFX=
  for ((i = 0; i < $1; i++)); do UI_PFX+="  │ "; done
  _agent_loop "$@"
  rc=$?
  UI_PFX=$saved_pfx
  return $rc
}

_agent_loop() {
  local depth=$1 kind=$2 base=$SPAN_D step=0 tools attrs content n i rc result SYSTEM_TEXT=
  local -a ids names args
  AGENT_OUT= AGENT_ERR=
  if ((depth < MAX_DEPTH)); then tools=$TOOLS_ALL; else tools=$TOOLS_BASE; fi
  if [[ $kind == main ]]; then
    attrs='"neatlogs.agent.name":"shy","neatlogs.agent.role":"coding agent: decides the next model call or tool call"'
  else
    attrs='"neatlogs.agent.name":"shy-subagent","neatlogs.agent.role":"subagent: completes one delegated task with a fresh context"'
  fi
  [[ -n $TRACE ]] && json_parse "${msgs[0]}" && jget content && SYSTEM_TEXT=$REPLY

  while :; do
    step=$((step + 1))
    if ((step > MAX_STEPS)); then
      AGENT_ERR="stopped after $MAX_STEPS model calls (SHY_MAX_STEPS)"
      span_unwind "$base" ERROR "$AGENT_ERR"
      return 1
    fi
    span_open agent-turn AGENT "\"attributes\":{$attrs}"
    llm_request "$tools"
    rc=$?
    if ((rc != 0)); then
      AGENT_ERR=$L_ERR
      span_unwind "$base" ERROR "$L_ERR"
      return $rc
    fi
    content=$L_CONTENT n=$L_TC_N
    ids=("${L_TC_ID[@]}")
    names=("${L_TC_NAME[@]}")
    args=("${L_TC_ARGS[@]}")
    msgs+=("$L_MSG")

    if [[ -n $L_REASONING ]]; then
      result=${L_REASONING:0:600}
      oneline "${result//$'\n'/ }" 300
      note "✻ $REPLY"
    fi
    if [[ -n $content ]]; then
      if ((depth == 0)); then
        printf '%s\n' "$content"
      elif ((n > 0)); then
        show_lines "$content" 3 "$UI_PFX"
      fi
    fi
    [[ $L_FINISH == length ]] && warn "(the response hit the output token limit)"

    if ((n == 0)); then
      AGENT_OUT=$content
      json_esc "$content"
      span_close OK "$REPLY"
      return 0
    fi

    for ((i = 0; i < n; i++)); do
      if [[ -n $INTERRUPTED ]]; then
        TOOL_OUT="Not run: interrupted by user."
      else
        run_tool "${names[i]}" "${args[i]}" "$depth"
      fi
      json_esc "${ids[i]}"
      result="{\"role\":\"tool\",\"tool_call_id\":$REPLY"
      json_esc "$TOOL_OUT"
      msgs+=("$result,\"content\":$REPLY}")
    done
    if [[ -n $INTERRUPTED ]]; then
      AGENT_ERR="interrupted by user"
      span_unwind "$base" ERROR "$AGENT_ERR"
      return 130
    fi
    if [[ -n $content ]]; then json_esc "$content" && span_close OK "$REPLY"; else span_close OK; fi
  done
}

# ---------------------------------------------------------------- entry points

# One user request: one Neatlogs trace. Uses the caller's msgs array.
run_turn() { # workflow prompt
  local workflow=$1 prompt=$2 rc t0=$SECONDS cost
  INTERRUPTED= IN_TURN=1
  U_IN=0 U_OUT=0 U_CALLS=0 U_MICROS=0
  trace_begin "$workflow" "$prompt"
  json_esc "$prompt"
  msgs+=("{\"role\":\"user\",\"content\":$REPLY}")
  agent_run 0 main
  rc=$?
  case $rc in
    0) trace_end OK "$AGENT_OUT" ;;
    130) trace_end ERROR "" "interrupted by user" ;;
    *) trace_end ERROR "" "$AGENT_ERR" ;;
  esac
  IN_TURN=
  ((rc == 1)) && printf '%sError: %s%s\n' "$C_RED" "$AGENT_ERR" "$C_RST" >&2
  ((rc == 130)) && warn "Interrupted."
  printf -v cost '$%d.%04d' $((U_MICROS / 1000000)) $(((U_MICROS % 1000000) / 100))
  fmt_k "$U_IN"
  local tokens_in=$REPLY
  fmt_k "$U_OUT"
  note "· $MODEL · $U_CALLS calls · $tokens_in in / $REPLY out · $cost · $((SECONDS - t0))s"
  return $rc
}

on_int() {
  if [[ -z $IN_TURN ]]; then
    PROMPT_INTERRUPTED=1
    return
  fi
  if [[ -n $INTERRUPTED ]]; then
    EXIT_REASON="interrupted by user"
    exit 130
  fi
  INTERRUPTED=1
  [[ -n $CHILD_PID ]] && kill_tree "$CHILD_PID" TERM
}

on_exit() {
  local rc=$?
  trap - EXIT INT
  [[ -n $CHILD_PID ]] && kill_tree "$CHILD_PID" TERM
  if [[ -n $TRACE ]] && ((SPAN_D >= 0)); then
    trace_end ERROR "" "${EXIT_REASON:-shy exited with status $rc before the run finished}"
  fi
  [[ -n ${SHY_TMP:-} && -d $SHY_TMP ]] && rm -rf "$SHY_TMP"
  exit "$rc"
}

ensure_api_key() {
  [[ -n ${OPENROUTER_API_KEY:-} ]] && return 0
  [[ -n $HAVE_TTY ]] || die "OPENROUTER_API_KEY is not set and there is no terminal to ask for it."
  printf '%sOpenRouter API key%s (https://openrouter.ai/keys, input hidden): ' "$C_BOLD" "$C_RST" >/dev/tty
  IFS= read -r -s OPENROUTER_API_KEY </dev/tty
  printf '\n' >/dev/tty
  OPENROUTER_API_KEY=${OPENROUTER_API_KEY//[[:space:]]/}
  [[ -n $OPENROUTER_API_KEY ]] || die "no API key given."
  note "Tip: export OPENROUTER_API_KEY to skip this prompt."
}

read_prompt() {
  local line rc
  PROMPT_LINE= PROMPT_INTERRUPTED=
  while :; do
    locale_user
    IFS= read -r -e -p $'\001'"$C_BOLD"$'\002''› '$'\001'"$C_RST"$'\002' line </dev/tty
    rc=$?
    LC_ALL=C
    if ((rc != 0)); then
      if [[ -n $PROMPT_INTERRUPTED ]] || ((rc > 128)); then
        printf '\n' >&2
        PROMPT_LINE= PROMPT_INTERRUPTED=
        continue
      fi
      return 1
    fi
    [[ -n $line ]] && history -s -- "$line" 2>/dev/null
    if [[ $line == *"$J_B" ]]; then
      PROMPT_LINE+="${line%?}"$'\n'
      continue
    fi
    PROMPT_LINE+=$line
    return 0
  done
}

new_session() {
  new_id
  SESSION_ID=${NEATLOGS_SESSION_ID:-$REPLY}
  system_prompt main 0
  json_esc "$REPLY"
  msgs=("{\"role\":\"system\",\"content\":$REPLY}")
}

repl() {
  local -a msgs
  local tracing="off (set NEATLOGS_API_KEY to send traces)"
  new_session
  [[ -n $TRACE ]] && tracing="Neatlogs, session $SESSION_ID"
  printf '%sshy %s%s %s· %s · %s%s\n' "$C_BOLD" "$SHY_VERSION" "$C_RST" "$C_DIM" "$MODEL" "$PWD" "$C_RST" >&2
  note "tracing: $tracing"
  note "/help for commands, Ctrl-C interrupts, Ctrl-D exits"
  set -o history 2>/dev/null
  HISTFILE=
  while :; do
    printf '\n' >&2
    read_prompt || break
    [[ -z ${PROMPT_LINE//[[:space:]]/} ]] && continue
    case $PROMPT_LINE in
      /exit | /quit) break ;;
      /new | /clear)
        new_session
        note "Started a new conversation."
        continue
        ;;
      /model) note "model: $MODEL" && continue ;;
      "/model "*)
        MODEL=${PROMPT_LINE#/model }
        note "model: $MODEL"
        continue
        ;;
      /help)
        note "/new      start a new conversation"
        note "/model X  switch to model X (OpenRouter id)"
        note "/exit     quit (or Ctrl-D)"
        note "End a line with \\ to keep typing on the next line."
        continue
        ;;
      /*) warn "Unknown command: $PROMPT_LINE (try /help)" && continue ;;
    esac
    printf '\n' >&2
    run_turn chat-turn "$PROMPT_LINE"
  done
  printf '\n' >&2
}

usage() {
  cat >&2 <<EOF
shy $SHY_VERSION, a coding agent in one bash file.

Usage: shy.sh [options] [task...]
  With a task, runs it once and prints the final answer on stdout.
  Without one, starts an interactive session.

Options:
  -m, --model ID   OpenRouter model id (default: $MODEL)
  -h, --help       show this help
  -v, --version    print the version

Tools: read, edit, write, bash, webfetch, subagent.
EOF
}

main() {
  setup
  local -a words
  while (($#)); do
    case $1 in
      -m | --model)
        [[ -n ${2:-} ]] || die "$1 needs a model id"
        MODEL=$2
        shift 2
        ;;
      -h | --help) usage && exit 0 ;;
      -v | --version) echo "shy $SHY_VERSION" && exit 0 ;;
      --) shift && words+=("$@") && break ;;
      -*) die "unknown option $1 (see --help)" ;;
      *) words+=("$1") && shift ;;
    esac
  done
  command -v curl >/dev/null 2>&1 || die "curl is required."
  SHY_TMP=$(mktemp -d "${TMPDIR:-/tmp}/shy.XXXXXX") || die "could not create a temp directory."
  trap on_exit EXIT
  trap on_int INT
  trap 'EXIT_REASON="terminated"; exit 143' TERM HUP
  ensure_api_key
  trace_init

  if ((${#words[@]})); then
    local -a msgs
    SESSION_ID=${NEATLOGS_SESSION_ID:-}
    system_prompt main 0
    json_esc "$REPLY"
    msgs=("{\"role\":\"system\",\"content\":$REPLY}")
    run_turn run-task "${words[*]}"
    exit $?
  fi
  [[ -n $HAVE_TTY ]] || die "no task given and no terminal for an interactive session."
  repl
  exit 0
}

main "$@"
