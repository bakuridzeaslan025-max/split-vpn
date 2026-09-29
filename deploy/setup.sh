#!/bin/sh
# One-time setup of a clean Debian/Ubuntu server, run as root by `make setup`:
# Docker from its official apt repo and a firewall with only ssh/80/443 open.
set -eu

export DEBIAN_FRONTEND=noninteractive
apt-get update -q
apt-get install -yq ca-certificates curl ufw

if ! docker compose version >/dev/null 2>&1; then
    . /etc/os-release
    install -m 0755 -d /etc/apt/keyrings
    curl -fsSL "https://download.docker.com/linux/$ID/gpg" -o /etc/apt/keyrings/docker.asc
    echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/$ID $VERSION_CODENAME stable" \
        > /etc/apt/sources.list.d/docker.list
    apt-get update -q
    apt-get install -yq docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin
fi

# The port this ssh session came in on, so a non-standard sshd port does not
# lock us out.
ssh_port=${SSH_CONNECTION:+${SSH_CONNECTION##* }}
ufw default deny incoming
ufw default allow outgoing
ufw allow "${ssh_port:-22}/tcp"
ufw allow 80/tcp
ufw allow 443/tcp
ufw --force enable

docker compose version
