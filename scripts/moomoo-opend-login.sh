#!/bin/sh
# Root-owned interactive launcher. Credentials go only to the vendor terminal.
set -eu
test "$(id -u)" -eq 0
test "$#" -eq 0
exec systemd-run --quiet --collect --wait --pty \
  --unit=autowheel-moomoo-opend-qualify \
  --uid=autowheel-moomoo-opend \
  --working-directory=/var/lib/autowheel-moomoo-opend \
  --property=UMask=0077 \
  --property=NoNewPrivileges=yes \
  --property=PrivateTmp=yes \
  --property=ProtectSystem=strict \
  --property=ProtectHome=yes \
  --property=ReadWritePaths=/var/lib/autowheel-moomoo-opend \
  --property=RestrictAddressFamilies='AF_UNIX AF_INET AF_INET6' \
  /opt/autowheel-moomoo-opend/10.11.7108/OpenD \
  -cfg_file=/var/lib/autowheel-moomoo-opend/OpenD.xml \
  -api_ip=127.0.0.1 -api_port=11112 -log_level=no -no_monitor=1 \
  -auto_hold_quote_right=0 -price_reminder_push=0 -console=1
