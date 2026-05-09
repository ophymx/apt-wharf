#!/bin/sh
set -e

if [ -d /run/systemd/system ]; then
    systemctl --no-reload disable signpost.service >/dev/null 2>&1 || true
    systemctl stop signpost.service >/dev/null 2>&1 || true
fi
