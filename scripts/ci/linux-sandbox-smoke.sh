#!/usr/bin/env bash
set -euo pipefail

[[ $(uname -s) == Linux && $(id -u) != 0 ]] || { echo 'Smoke must run as the non-root test user' >&2; exit 1; }
: "${XDG_RUNTIME_DIR:?}"
: "${DBUS_SESSION_BUS_ADDRESS:?}"

bwrap_path=$(command -v bwrap)
bwrap --version
[[ -f $bwrap_path && ! -u $bwrap_path && ! -g $bwrap_path ]]
[[ $(stat -c %u "$bwrap_path") == 0 ]]
[[ -z $(getcap "$bwrap_path") ]]
stat -c 'sandbox executable: %U:%G %a %n' "$bwrap_path"
printf 'Host AppArmor label: '
cat /proc/self/attr/current
for key in kernel.unprivileged_userns_clone kernel.apparmor_restrict_unprivileged_userns kernel.apparmor_restrict_unprivileged_unconfined user.max_user_namespaces; do
  if [[ -r /proc/sys/${key//./\/} ]]; then
    sysctl "$key"
  fi
done

# No network probe leaves this machine. A live host loopback listener is the
# negative control: connection would succeed if bwrap reused the host network.
/usr/bin/python3 -I - "$bwrap_path" <<'PY'
import errno
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile

names = ('user', 'pid', 'mnt', 'net', 'ipc', 'uts', 'cgroup')
host = {name: os.readlink('/proc/self/ns/' + name) for name in names}
host['pid_value'] = os.getpid()
host['uid'] = os.getuid()
if host['uid'] == 0:
    raise SystemExit('root is not a valid sandbox test identity')

inner = r'''
import errno, json, os, socket, sys
from pathlib import Path
host = json.loads(sys.argv[1])
marker = sys.argv[2]
port = int(sys.argv[3])
observed = {name: os.readlink('/proc/self/ns/' + name) for name in ('user', 'pid', 'mnt', 'net', 'ipc', 'uts', 'cgroup')}
for name, value in observed.items():
    if value == host[name]:
        raise SystemExit(name + ' namespace was not isolated')
if Path('/proc/' + str(host['pid_value'])).exists():
    raise SystemExit('host process is visible in sandbox procfs')
for path in (marker, '/etc/shadow', '/root', '/run/user', '/var/run/docker.sock'):
    try:
        os.listdir(path) if path in ('/root', '/run/user') else Path(path).read_bytes()
    except (FileNotFoundError, PermissionError, NotADirectoryError):
        pass
    else:
        raise SystemExit('host-sensitive path is accessible: ' + path)
try:
    fd = os.open('/usr/.lc-ci-write-probe', os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
except OSError as e:
    if e.errno != errno.EROFS:
        raise SystemExit('expected a read-only system mount, got ' + str(e))
else:
    os.close(fd)
    os.unlink('/usr/.lc-ci-write-probe')
    raise SystemExit('system mount is writable')
Path('/tmp/private-ci-probe').write_text('private scratch')
if Path('/tmp/private-ci-probe').read_text() != 'private scratch':
    raise SystemExit('private scratch is not usable')
with socket.socket() as s:
    s.settimeout(2)
    if s.connect_ex(('127.0.0.1', port)) == 0:
        raise SystemExit('sandbox reached the host loopback listener')
if any(name != 'lo' for _, name in socket.if_nameindex()):
    raise SystemExit('host network interface leaked into sandbox')
with socket.socket() as private:
    private.bind(('127.0.0.1', 0))
    private.listen(1)
    with socket.create_connection(private.getsockname(), timeout=2):
        pass
status = dict(line.split(':', 1) for line in Path('/proc/self/status').read_text().splitlines() if ':' in line)
if int(status['CapEff'].strip(), 16) != 0 or status['NoNewPrivs'].strip() != '1':
    raise SystemExit('sandbox retained effective capabilities or lacks no_new_privs')
print(json.dumps({'result': 'PASS', 'namespaces': observed, 'uid_map': Path('/proc/self/uid_map').read_text().strip(), 'host_uid': host['uid'], 'sandbox_uid': os.getuid(), 'host_pid_hidden': True, 'host_sensitive_paths_denied': True, 'system_mount_read_only': True, 'private_scratch_usable': True, 'host_network_denied': True, 'private_loopback_usable': True, 'CapEff': status['CapEff'].strip(), 'NoNewPrivs': status['NoNewPrivs'].strip(), 'apparmor': Path('/proc/self/attr/current').read_text().strip()}, sort_keys=True))
'''

with tempfile.TemporaryDirectory(prefix='.lc-ci-host-sensitive-', dir=str(Path.home())) as directory, socket.socket() as listener:
    marker = Path(directory) / 'synthetic-secret'
    marker.write_text('synthetic CI marker; not a credential')
    if not marker.read_text():
        raise SystemExit('host negative-control marker is unreadable before sandboxing')
    listener.bind(('127.0.0.1', 0))
    listener.listen(1)
    args = [sys.argv[1], '--die-with-parent', '--unshare-user', '--unshare-pid', '--unshare-ipc', '--unshare-uts', '--unshare-cgroup', '--unshare-net', '--new-session', '--cap-drop', 'ALL', '--clearenv']
    for path in ('/usr', '/bin', '/sbin', '/lib', '/lib64'):
        if Path(path).exists():
            args += ['--ro-bind', path, path]
    args += ['--proc', '/proc', '--dev', '/dev', '--tmpfs', '/tmp', '--tmpfs', '/run', '--dir', '/var', '--symlink', '/run', '/var/run', '--dir', '/etc', '--setenv', 'PATH', '/usr/bin:/bin', '--setenv', 'HOME', '/tmp', '--', '/usr/bin/python3', '-I', '-', json.dumps(host), str(marker), str(listener.getsockname()[1])]
    print('HOST ' + json.dumps(host, sort_keys=True), flush=True)
    subprocess.run(args, input=inner, text=True, check=True, timeout=30)
PY

# Same user manager/delegation mechanism as internal/cgroupv2.NewPool. The Go
# isolation tests subsequently prove worker admission and hard memory limits.
systemd-run --user --scope --quiet --collect --property=Delegate=yes -- \
  /bin/bash -euo pipefail -c '
    relative=$(awk -F: '\''$1 == "0" {print $3}'\'' /proc/self/cgroup)
    scope=/sys/fs/cgroup$relative
    [[ -w $scope/cgroup.procs && -w $scope/cgroup.subtree_control ]]
    for controller in memory pids; do
      grep -qw "$controller" "$scope/cgroup.controllers"
    done
    printf "Delegated user scope: %s\n" "$relative"
    printf "Controllers: "
    cat "$scope/cgroup.controllers"
  '
echo 'Linux namespace and delegated-cgroup smoke: PASS'
