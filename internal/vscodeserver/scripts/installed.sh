#!/bin/sh
# $1 = data folder, $2 = install key, $3 = server binary. Prints yes or no.
if [ -x "$HOME/$1/bin/$2/bin/$3" ]; then echo yes; else echo no; fi
