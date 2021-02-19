#!/bin/sh
# enable debug logging
set -x
# abort if subshell command exits non-zero
set -e

# see `man 5 os-release` and http://0pointer.de/blog/projects/os-release
. /etc/os-release

. $(dirname $(readlink -f $0))/utils.sh

LXC_PPA=${LXC_PPA:-http://ppa.launchpad.net/ubuntu-lxc/lxc-git-master/ubuntu}
LXC_PPA_KEY=${LXC_PPA_KEY:-93763AC528C8C52568951BE0D5495F657635B973}
LXC_PPA_KEYURL="${LXC_PPA_KEYURL:-http://keyserver.ubuntu.com/pks/lookup?op=get&search=0x$LXC_PPA_KEY}"
LXC_PPA_DEPS="curl gnupg2 ca-certificates"

install_lxc_ppa() {
	curl -sSL "$LXC_PPA_KEYURL" | apt-key add - >/dev/null
	echo "deb $LXC_PPA $UBUNTU_CODENAME main" >/etc/apt/sources.list.d/lxc-git-master.list
	apt-get update
	apt_install lxc
}

LXC_BUILD_TOOLS="build-essential libtool automake pkg-config git ca-certificates"
LXC_BUILD_LIBS="libseccomp-dev libapparmor-dev libbtrfs-dev libdevmapper-dev libcap-dev libc6-dev libglib2.0-dev"
LXC_BUILD_DEPS="$LXC_BUILD_TOOLS $LXC_BUILD_LIBS"
LXC_RUNTIME_DEPS="libseccomp2 libapparmor1 libbtrfs0 libdevmapper1.02.1 libcap2"

case $LXC_INSTALL_FROM in
"git")
	apt_install $LXC_BUILD_DEPS $LXC_RUNTIME_DEPS
	install_lxc_git
	apt_clean $LXC_BUILD_DEPS
	;;
"ppa")
	apt_install $LXC_PPA_DEPS
	install_lxc_ppa
	apt_clean $LXC_PPA_DEPS
	;;
*)
	echo "Installation method 'LXC_INSTALL_FROM=$LXC_INSTALL_FROM' is unsupported" >&2
	echo "Supported installation methods are: 'git' and 'ppa'" >&2
	exit 1
	;;
esac
