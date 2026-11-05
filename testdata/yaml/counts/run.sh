#!/bin/sh
home=$1
dir=$2
mkdir -p "$home/items"
printf 'start after %s\n' "x" >> "$dir/runs.txt"
printf '{}\n' > "$home/items/item_1.json"
sleep 30
