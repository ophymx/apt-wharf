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

if [ -d /run/systemd/system ]; then
    systemctl daemon-reload >/dev/null 2>&1 || true
    # try-restart is a no-op when the unit isn't running, so this picks
    # up the new binary on upgrade without starting signpost on a fresh
    # install (where the operator hasn't enabled the unit yet).
    systemctl try-restart signpost.service >/dev/null 2>&1 || true
fi
