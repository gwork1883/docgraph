#!/usr/bin/env bash
set -euo pipefail

# DocGraph startup script
# Usage: ./run.sh {serve|start|stop|restart|status} [flags...]

# Resolve the directory containing this script
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BIN="${SCRIPT_DIR}/bin/docgraph"
CONFIG="${SCRIPT_DIR}/docgraph.yaml"
DATA="${SCRIPT_DIR}/.docgraph"
PID_FILE="${SCRIPT_DIR}/.docgraph.pid"
LOG_FILE="${SCRIPT_DIR}/.docgraph.log"

if [ ! -f "$BIN" ]; then
    echo "ERROR: binary not found at $BIN"
    exit 1
fi
if [ ! -f "$CONFIG" ]; then
    echo "ERROR: config not found at $CONFIG"
    exit 1
fi

ACTION="${1:-serve}"
shift 2>/dev/null || true

# ---------------------------------------------------------------------------
# helpers
# ---------------------------------------------------------------------------
is_running() {
    if [ -f "$PID_FILE" ]; then
        local pid
        pid="$(cat "$PID_FILE")"
        if kill -0 "$pid" 2>/dev/null; then
            return 0
        fi
    fi
    return 1
}

get_pid() {
    if [ -f "$PID_FILE" ]; then
        cat "$PID_FILE"
    fi
}

do_start() {
    if is_running; then
        echo "docgraph is already running (pid $(get_pid))"
        return 1
    fi

    echo "Starting docgraph in background..."
    nohup "$BIN" serve --config "$CONFIG" "$@" >> "$LOG_FILE" 2>&1 &
    local pid=$!
    echo "$pid" > "$PID_FILE"

    # Wait a moment and verify it started
    sleep 1
    if kill -0 "$pid" 2>/dev/null; then
        echo "docgraph started (pid $pid)"
    else
        echo "ERROR: docgraph failed to start. Check $LOG_FILE"
        rm -f "$PID_FILE"
        return 1
    fi
}

do_stop() {
    if ! is_running; then
        echo "docgraph is not running"
        rm -f "$PID_FILE"
        return 0
    fi

    local pid
    pid="$(get_pid)"
    echo "Stopping docgraph (pid $pid)..."
    kill "$pid" 2>/dev/null || true

    # Wait up to 10s for graceful shutdown
    local waited=0
    while kill -0 "$pid" 2>/dev/null && [ "$waited" -lt 10 ]; do
        sleep 1
        waited=$((waited + 1))
    done

    if kill -0 "$pid" 2>/dev/null; then
        echo "Graceful shutdown timed out, force killing..."
        kill -9 "$pid" 2>/dev/null || true
        sleep 1
    fi

    rm -f "$PID_FILE"
    echo "docgraph stopped"
}

# ---------------------------------------------------------------------------
# dispatch
# ---------------------------------------------------------------------------
case "$ACTION" in
    serve)
        # Foreground run — logs go to stdout/stderr
        exec "$BIN" serve --config "$CONFIG" "$@"
        ;;
    start)
        do_start "$@"
        ;;
    stop)
        do_stop
        ;;
    restart)
        do_stop
        sleep 1
        do_start "$@"
        ;;
    status)
        if is_running; then
            echo "docgraph is running (pid $(get_pid))"
        else
            echo "docgraph is not running"
        fi
        # Also show the built-in status if the DB exists
        if [ -d "$DATA" ]; then
            echo ""
            "$BIN" status --config "$CONFIG" --data "$DATA" "$@" 2>/dev/null || true
        fi
        ;;
    logs)
        if [ -f "$LOG_FILE" ]; then
            exec tail -f "$LOG_FILE"
        else
            echo "No log file at $LOG_FILE"
            exit 1
        fi
        ;;
    *)
        echo "Usage: $0 {serve|start|stop|restart|status|logs} [flags...]"
        echo ""
        echo "  serve    Run in foreground (logs to stdout)"
        echo "  start    Start in background"
        echo "  stop     Stop background instance"
        echo "  restart  Restart background instance"
        echo "  status   Show running status + DB stats"
        echo "  logs     Tail the background log file"
        exit 1
        ;;
esac
