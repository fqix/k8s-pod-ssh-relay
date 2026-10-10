#!/bin/sh
# $1 = data folder, $2 = install key. Reads the server tarball from stdin and
# unpacks it to $HOME/$1/bin/$2. Microsoft tarballs have one top-level
# directory, VSCodium's have none.
set -e
BASE="$HOME/$1/bin"; DIR="$BASE/$2"
mkdir -p "$BASE"
TMP=$(mktemp -d "$BASE/.upload.XXXXXX")
trap 'rm -rf "$TMP"' EXIT
tar -xzf - -C "$TMP"
set -- "$TMP"/*
rm -rf "$DIR"
if [ $# -eq 1 ] && [ -d "$1" ]; then mv "$1" "$DIR"; else mkdir "$DIR" && mv "$TMP"/* "$DIR"/; fi
