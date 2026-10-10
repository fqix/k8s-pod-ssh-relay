#!/bin/sh
# Prints: uname -s, uname -m, musl|glibc, $HOME.
printf '%s %s ' "$(uname -s)" "$(uname -m)"
if [ -f /etc/alpine-release ] || ldd --version 2>&1 | grep -qi musl; then printf 'musl '; else printf 'glibc '; fi
printf '%s\n' "$HOME"
