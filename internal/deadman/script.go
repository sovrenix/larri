// Copyright (C) 2026 Sovrenix Inc.
// SPDX-License-Identifier: GPL-3.0-or-later

package deadman

// script is the watchdog.
//
// POSIX sh and nothing else, deliberately: it has to run on whatever minimal
// image a marketplace host happens to provide, and a dependency on bash or
// python is a dependency that will one day not be there — on the one code path
// whose whole job is to work after everything else has failed. Every probe is
// guarded so that a missing tool reads as "no evidence", never as an error
// that kills the loop.
//
// Both timestamps come from the host's own clock, so there is no skew to
// reason about. LARRI's clock never enters into it.
const script = `#!/bin/sh
# LARRI dead-man switch.
#
# Halts this container when LARRI has stopped checking in AND the host is
# doing nothing worth keeping. Two signals, never one: a missed heartbeat
# means the operator is gone, not that the work is finished.
#
# Installed from the operator's machine. See /var/log/larri-watchdog.log.

BEAT=/var/run/larri/heartbeat
DEADLINE="${LARRI_DEADLINE:-1800}"
PORT="${LARRI_PORT:-8000}"
RTLOG="${LARRI_RUNTIME_LOG:-/dev/null}"
MAX_GRACE="${LARRI_MAX_GRACE:-7200}"
LOG=/var/log/larri-watchdog.log
INTERVAL=30

log() { echo "$(date -u +%Y-%m-%dT%H:%M:%SZ) $*" >> "$LOG" 2>/dev/null; }

# --- activity probes -------------------------------------------------------
# Each answers "is there evidence of work?" and prints a reason when yes.
# Absent tools print nothing, which is the safe reading: no evidence.

gpu_busy() {
    u=$(nvidia-smi --query-gpu=utilization.gpu --format=csv,noheader,nounits 2>/dev/null \
        | tr -d ' ' | sort -rn | head -1)
    [ -n "$u" ] && [ "$u" -ge 5 ] 2>/dev/null && echo "gpu ${u}%"
}

# A request in flight keeps its connection open, so an established socket on
# the runtime port is a generation still running for a client whose tunnel
# dropped. Killing that is killing work someone is waiting on.
conns_busy() {
    n=$(ss -Htn state established "( sport = :$PORT )" 2>/dev/null | wc -l)
    [ -z "$n" ] || [ "$n" = "0" ] && n=$(netstat -tn 2>/dev/null | grep -c ":$PORT .*ESTABLISHED")
    [ -n "$n" ] && [ "$n" -gt 0 ] 2>/dev/null && echo "${n} open connection(s)"
}

# A growing runtime log is the signal that carried the readiness wait, and it
# carries this one for the same reason: a weight download and a checkpoint
# load both talk while the counters stay quiet.
log_grew() {
    [ -f "$RTLOG" ] || return 0
    cur=$(stat -c %s "$RTLOG" 2>/dev/null || echo 0)
    [ -n "$PREV_LOG" ] && [ "$cur" -gt "$PREV_LOG" ] 2>/dev/null && echo "runtime log +$((cur - PREV_LOG))B"
    PREV_LOG="$cur"
}

# Disk and network, for the phases that log nothing: extracting an archive,
# or a download that reports only at the end.
io_busy() {
    [ -r /proc/net/dev ] || return 0
    cur=$(awk 'NR>2 {rx+=$2; tx+=$10} END {print rx+tx}' /proc/net/dev 2>/dev/null)
    [ -n "$PREV_NET" ] && [ -n "$cur" ] && \
        [ "$((cur - PREV_NET))" -gt $((5 * 1024 * 1024)) ] 2>/dev/null && \
        echo "net $(( (cur - PREV_NET) / 1024 / 1024 ))MB"
    PREV_NET="$cur"
}

busy_reason() {
    r=""
    for probe in gpu_busy conns_busy log_grew io_busy; do
        out=$($probe 2>/dev/null)
        [ -n "$out" ] && r="$r${r:+, }$out"
    done
    echo "$r"
}

# --- main ------------------------------------------------------------------

log "armed: deadline ${DEADLINE}s, runtime port ${PORT}, max grace ${MAX_GRACE}s"
PREV_LOG=""
PREV_NET=""
GRACE=0

# Prime the deltas so the first pass does not read a cold start as activity.
log_grew >/dev/null
io_busy  >/dev/null

while :; do
    sleep "$INTERVAL"

    last=$(stat -c %Y "$BEAT" 2>/dev/null)
    # A missing heartbeat file means disarmed, not abandoned: Disarm removes
    # it deliberately before a teardown LARRI is performing itself.
    if [ -z "$last" ]; then
        log "heartbeat gone; standing down"
        exit 0
    fi

    now=$(date +%s)
    age=$((now - last))
    reason=$(busy_reason)

    if [ "$age" -lt "$DEADLINE" ]; then
        GRACE=0
        continue
    fi

    # LARRI is gone. That opens the question; it does not answer it.
    if [ -n "$reason" ]; then
        GRACE=$((GRACE + INTERVAL))
        if [ "$GRACE" -lt "$MAX_GRACE" ]; then
            log "larri absent ${age}s but host busy ($reason) — waiting (grace ${GRACE}s/${MAX_GRACE}s)"
            continue
        fi
        log "larri absent ${age}s, still busy ($reason) but grace exhausted after ${GRACE}s"
    else
        log "larri absent ${age}s and host idle — stopping"
    fi

    # The runtime first, so VRAM is released even if the halt below is
    # refused. On a host where nothing else works, this at least ends the
    # work the GPU is doing.
    pkill -f '[v]llm serve' >/dev/null 2>&1
    pkill -f '[v]llm\.entrypoints\.openai' >/dev/null 2>&1
    pkill -f '[l]lama-server' >/dev/null 2>&1
    pkill -f '[o]llama serve' >/dev/null 2>&1
    log "runtime stopped"

    # Then attempt the container. A marketplace instance *is* the container
    # (§6.5.1), so ending PID 1 should end the instance — but Vast has been
    # observed to keep reporting such an instance as running, so this is an
    # attempt and is logged as one. The runtime kill above is the part that
    # reliably achieves something: the rig stops serving.
    sync
    log "attempting halt (billing may continue; destroy the instance to end it)"
    halt -f         >/dev/null 2>&1
    poweroff -f     >/dev/null 2>&1
    shutdown -h now >/dev/null 2>&1
    kill -TERM 1    >/dev/null 2>&1
    sleep 10
    kill -KILL 1    >/dev/null 2>&1
    exit 0
done
`
