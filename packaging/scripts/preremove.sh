#!/bin/sh
set -e

# dpkg runs the *old* package's prerm on upgrade too ($1 = "upgrade").
# Only tear the unit down on a real removal; on upgrade leave it
# running so postinstall's try-restart picks up the new binary.
if [ "$1" = "remove" ] && [ -d /run/systemd/system ]; then
    systemctl --no-reload disable signpost.service >/dev/null 2>&1 || true
    systemctl stop signpost.service >/dev/null 2>&1 || true
fi
