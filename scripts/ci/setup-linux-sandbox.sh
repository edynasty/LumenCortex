#!/usr/bin/env bash
set -euo pipefail

# Provision only the disposable GitHub-hosted VM. Tests never run under sudo.
[[ ${GITHUB_ACTIONS:-} == true ]] || { echo 'GitHub Actions is required' >&2; exit 1; }
[[ $(uname -s) == Linux && $(id -u) != 0 ]] || { echo 'A non-root Linux runner is required' >&2; exit 1; }
: "${GITHUB_ENV:?}"

printf 'Runner image: %s %s\n' "${ImageOS:-unknown}" "${ImageVersion:-unknown}"
cat /etc/os-release
uname -a
id
printf 'Initial PATH: %s\n' "$PATH"
if command -v bwrap; then
  bwrap --version
else
  echo 'bubblewrap is not executable on the initial PATH'
fi
for key in kernel.unprivileged_userns_clone kernel.apparmor_restrict_unprivileged_userns kernel.apparmor_restrict_unprivileged_unconfined user.max_user_namespaces; do
  if [[ -r /proc/sys/${key//./\/} ]]; then
    sysctl "$key"
  fi
done

# Use distro binaries/libraries: the runtime mounts /usr read-only, not the
# hosted toolcache Python's unrelated installation directory.
sudo apt-get update
sudo apt-get install --no-install-recommends -y \
  bubblewrap dbus-user-session libcap2-bin \
  python3 python3-numpy python3-scipy python3-pandas python3-matplotlib

uid=$(id -u)
user=$(id -un)
sudo loginctl enable-linger "$user"
sudo systemctl start "user@${uid}.service"
sudo systemctl set-property --runtime "user@${uid}.service" 'Delegate=cpu cpuset io memory pids'

export XDG_RUNTIME_DIR="/run/user/$uid"
export DBUS_SESSION_BUS_ADDRESS="unix:path=$XDG_RUNTIME_DIR/bus"
[[ -d $XDG_RUNTIME_DIR && -S $XDG_RUNTIME_DIR/bus ]]
[[ $(stat -c %u "$XDG_RUNTIME_DIR") == "$uid" ]]
systemctl --user show-environment >/dev/null
systemctl show "user@${uid}.service" --property=ActiveState --property=Delegate --property=DelegateControllers
printf 'XDG_RUNTIME_DIR=%s\nDBUS_SESSION_BUS_ADDRESS=%s\n' "$XDG_RUNTIME_DIR" "$DBUS_SESSION_BUS_ADDRESS" >>"$GITHUB_ENV"

command -v bwrap
bwrap --version
stat -c 'bwrap owner=%U group=%G mode=%a path=%n' "$(command -v bwrap)"
getcap "$(command -v bwrap)"
command -v node
node --version
/usr/bin/python3 -c 'import sys, numpy, scipy, pandas, matplotlib; print(sys.version); print("scientific Python dependencies available")'
# Do not change AppArmor profiles, userns sysctls, capabilities or setuid modes.
