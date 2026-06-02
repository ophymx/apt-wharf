#!/bin/sh
# vault-pgp-sign-shim.sh — wire signpost's external signing contract to
# vault-pgp-sign(1).
#
# Drop this script somewhere persistent (e.g. /usr/local/bin/) and point
# signpost at it:
#
#   signing:
#     external:
#       command: ["/usr/local/bin/vault-pgp-sign-shim.sh"]
#       key: "release-2025"
#       # Optional: pre-fetched cert. When unset, signpost runs
#       # `<command> export --key <key>` at startup to seed the keyring.
#       # pubkey_file: "/etc/signpost/release-2025.asc"
#       timeout: "30s"
#       env:
#         VAULT_ADDR: "https://vault.example.com:8200"
#         VAULT_ROLE_ID_FILE: "/etc/signpost/vault-role-id"
#         VAULT_SECRET_ID_FILE: "/etc/signpost/vault-secret-id"
#
# Signpost invokes this script with one of:
#
#   <shim> export      --key <name>
#   <shim> detach-sign --key <name> --in <path> --out <path>
#                      [--source-date-epoch <epoch>] [--digest-algo SHA256]
#   <shim> clear-sign  --key <name> --in <path> --out <path>
#                      [--source-date-epoch <epoch>] [--digest-algo SHA256]
#
# vault-pgp-sign(1) speaks the same CLI verbatim, so this shim is a
# pass-through whose only job is letting the operator override the
# binary location via $VAULT_PGP_SIGN.

set -eu

VAULT_PGP_SIGN="${VAULT_PGP_SIGN:-/usr/bin/vault-pgp-sign}"

exec "$VAULT_PGP_SIGN" "$@"
