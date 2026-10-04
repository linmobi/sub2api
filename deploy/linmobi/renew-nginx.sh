#!/bin/sh
set -eu
/usr/sbin/nginx -t -q
/bin/systemctl reload nginx
