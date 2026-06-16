#!/bin/bash

# https://www.garyhawkins.me.uk/custom-logo-on-uefi-boot-screen/

# Output the built OVMF firmware into the repo's data directory.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
OUT_DIR="$(cd "$SCRIPT_DIR/../.." && pwd)/data"
mkdir -p "$OUT_DIR"

sudo podman run --rm -v $PWD/in:/in -v "$OUT_DIR":/out --rm -it docker.io/library/debian:bookworm /bin/bash -c "
set -x
rm /out/*
apt-get update
echo 'deb-src http://deb.debian.org/debian bookworm main' >> /etc/apt/sources.list
echo 'deb-src http://deb.debian.org/debian-security bookworm-security main' >> /etc/apt/sources.list
echo 'deb-src http://deb.debian.org/debian bookworm-updates main' >> /etc/apt/sources.list
apt-get update
apt-get -y install fakeroot diffutils
apt-get -y build-dep ovmf
fakeroot -- bash << 'EOF'
apt-get source ovmf
cp /in/Logo.bmp ./edk2-2022.11/MdeModulePkg/Logo/Logo.bmp 
patch -p0 < /in/rules.patch 
apt-get --build source ovmf
mkdir unpacked
dpkg-deb -R ovmf_2022.11-6+deb12u2_all.deb unpacked
cp unpacked/usr/share/OVMF/OVMF_CODE.fd /out
cp unpacked/usr/share/OVMF/OVMF_VARS.fd /out
EOF
"
sudo chown -R $USER:$USER "$OUT_DIR"