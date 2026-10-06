#!/usr/bin/env bash
set -euo pipefail
if [[ $(id -u) != 0 || $(uname -m) != aarch64 ]]; then
  echo 'Run as root on Raspberry Pi OS Lite 64-bit (ARM64).' >&2
  exit 1
fi
if [[ $# != 1 ]]; then
  echo 'Usage: sudo scripts/install.sh EXTRACTED_RELEASE_DIRECTORY' >&2
  exit 1
fi
repo_dir=$(cd -- "$(dirname -- "$0")/.." && pwd)
bundle_dir=$(cd -- "$1" && pwd)
release_version=$(cat "$bundle_dir/VERSION")
if [[ ! $release_version =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  echo 'Invalid release version' >&2; exit 1
fi
for program in systemctl lsblk findmnt python3 pkaction udevadm; do
  command -v "$program" >/dev/null || { echo "Missing $program; see README prerequisites" >&2; exit 1; }
done
for program in dozor mediamtx ffmpeg ffprobe; do
  test -x "$bundle_dir/bin/$program" || { echo "Missing $program" >&2; exit 1; }
done
if [[ -e /opt/dozor/current ]]; then
  echo 'Dozor is already installed. Use the signed updater for further releases.' >&2; exit 1
fi
id dozor >/dev/null 2>&1 || useradd --system --home-dir /var/lib/dozor --shell /usr/sbin/nologin dozor
install -d -o root -g root -m 0755 /opt/dozor/releases /etc/dozor /usr/local/libexec
install -d -o dozor -g dozor -m 0700 /var/lib/dozor
# Never chmod/chown an existing mount as if it were an empty directory.
if ! mountpoint -q /srv/dozor; then install -d -o root -g root -m 0555 /srv/dozor; fi
cp -a "$bundle_dir" "/opt/dozor/releases/$release_version"
chown -R root:root "/opt/dozor/releases/$release_version"
chmod 0755 "/opt/dozor/releases/$release_version" "/opt/dozor/releases/$release_version/bin"
ln -s "releases/$release_version" /opt/dozor/current
install -o root -g root -m 0755 "$bundle_dir/bin/dozor" /usr/local/libexec/dozor-system
install -o root -g root -m 0755 "$bundle_dir/bin/dozor" /usr/local/libexec/dozor-recover
for unit in dozor.service dozor-update.service dozor-update.timer dozor-recover.service dozor-rollback.service dozor-disk-prepare@.service; do
  install -m 0644 "$repo_dir/ops/$unit" "/etc/systemd/system/$unit"
done
install -d -m 0755 /etc/polkit-1/rules.d
install -m 0644 "$repo_dir/ops/50-dozor.rules" /etc/polkit-1/rules.d/50-dozor.rules
if [[ ! -e /var/lib/dozor/config.json ]]; then
  python3 - <<'PY'
import json, os, pwd
p='/var/lib/dozor/config.json'
config={'listen':'0.0.0.0:8080','archive':'/srv/dozor','disk_uuid':'','timezone':'Europe/Moscow','cameras':[],
        's3':{'enabled':False,'region':'us-east-1','prefix':'dozor','bytes_per_second':2097152},'auto_update':True}
fd=os.open(p, os.O_CREAT|os.O_EXCL|os.O_WRONLY,0o600)
with os.fdopen(fd,'w') as f: json.dump(config,f,indent=2)
u=pwd.getpwnam('dozor'); os.chown(p,u.pw_uid,u.pw_gid)
PY
fi
if [[ -d /etc/avahi/services ]]; then install -m 0644 "$repo_dir/ops/dozor-avahi.service" /etc/avahi/services/dozor.service; fi
systemctl daemon-reload
systemctl enable --now dozor.service dozor-update.timer
echo 'Open http://<raspberry-hostname>.local:8080 or the Raspberry IP address.'
echo 'First-run token: sudo cat /var/lib/dozor/setup-token'
echo 'For signed updates install your public key into /etc/dozor/update.pub (root:root 0644).'
