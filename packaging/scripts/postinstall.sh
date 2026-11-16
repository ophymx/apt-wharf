#!/bin/sh
set -e

if ! getent group signpost >/dev/null 2>&1; then
    groupadd --system signpost
fi
if ! getent passwd signpost >/dev/null 2>&1; then
    useradd --system --gid signpost \
            --home-dir /var/lib/signpost --no-create-home \
            --shell /usr/sbin/nologin \
            --comment "apt-signpost daemon" signpost
fi

chown root:signpost /etc/signpost
chmod 0750 /etc/signpost
if [ -f /etc/signpost/config.yaml ]; then
    chown root:signpost /etc/signpost/config.yaml
    chmod 0640 /etc/signpost/config.yaml
fi

chown signpost:signpost /var/lib/signpost
chmod 0750 /var/lib/signpost

# Packages before 0.6.0 shipped a prerm that stopped *and disabled*
# signpost.service on upgrade, not only on removal. dpkg runs the
# outgoing package's prerm, so an upgrade from one of them arrives
# here with the unit torn down, and try-restart below would be a
# no-op: the repo would stay dark until someone noticed.
#
# The prerm left no record of whether the unit had been enabled, so
# go by evidence: if signpost has written state under its state
# directory, it was in service — put it back. $2 is the version being
# upgraded from (empty on a fresh install). This only ever fires on
# that one upgrade: from 0.6.0 on, the outgoing prerm is the fixed one.
heal=0
if [ "$1" = "configure" ] && [ -n "$2" ] \
   && dpkg --compare-versions "$2" lt "0.6.0" \
   && [ -n "$(find /var/lib/signpost -mindepth 2 -print -quit 2>/dev/null)" ]; then
    heal=1
fi

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    if [ "$heal" = 1 ]; then
        echo "apt-signpost: the $2 package stopped and disabled signpost.service during this upgrade (fixed in 0.6.0); re-enabling and starting it."
        systemctl enable signpost.service >/dev/null 2>&1 || true
        systemctl start signpost.service >/dev/null 2>&1 \
            || echo "apt-signpost: signpost.service did not start; see 'systemctl status signpost.service'." >&2
    else
        # try-restart is a no-op when the unit isn't running, so this picks
        # up the new binary on upgrade without starting signpost on a fresh
        # install (where the operator hasn't enabled the unit yet).
        systemctl try-restart signpost.service >/dev/null 2>&1 || true
    fi
fi
