#!/bin/bash

hash hubr &>/dev/null || { echo "missing dep: hubr"; exit 1; }

hubr -h

for cmd in assets bump cat get install now push release resolve tags what; do
    printf ">----- %-66s -----<\\n" "sub: $cmd"
    hubr "$cmd" -h
done
