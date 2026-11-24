#!/bin/sh
set -eu

awk '$1 == "nameserver" { print "resolver " ($2 ~ /:/ ? "[" $2 "]" : $2) " valid=5s;"; found=1; exit } END { if (!found) exit 1 }' /etc/resolv.conf > /etc/nginx/resolver.inc
