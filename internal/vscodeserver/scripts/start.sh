#!/bin/sh
# $1 = data folder, $2 = install key, $3 = server binary. Reuses a running
# server, otherwise starts one; waits until it listens and prints its port
# and connection token.
DIR="$HOME/$1/bin/$2"; STATE="$HOME/$1/relay-$2"
mkdir -p "$STATE"
TOKEN="$STATE/token"; LOG="$STATE/server.log"; PIDF="$STATE/server.pid"
if [ ! -s "$TOKEN" ]; then (umask 077; od -An -N16 -tx1 /dev/urandom | tr -d ' \n' > "$TOKEN"); fi
running() { [ -f "$PIDF" ] && kill -0 "$(cat "$PIDF")" 2>/dev/null; }
port() { sed -n 's/.*Extension host agent listening on \([0-9][0-9]*\).*/\1/p' "$LOG" 2>/dev/null | tail -n 1; }
if ! running; then
  : > "$LOG"
  nohup "$DIR/bin/$3" --host=127.0.0.1 --port=0 --connection-token-file "$TOKEN" \
    --telemetry-level off --enable-remote-auto-shutdown --accept-server-license-terms >> "$LOG" 2>&1 < /dev/null &
  echo $! > "$PIDF"
fi
i=0; P=""
while [ $i -lt 300 ]; do
  P=$(port); [ -n "$P" ] && break
  running || break
  sleep 0.2; i=$((i+1))
done
if [ -z "$P" ]; then echo "VS Code server did not start; last log lines:" >&2; tail -n 30 "$LOG" >&2; exit 1; fi
echo "port=$P"
echo "token=$(cat "$TOKEN")"
