#!/usr/bin/env bash
set -euo pipefail

echo "Configuring system settings..."

# -------------------------------------------------
# 1. Create necessary directories
# -------------------------------------------------
echo "Creating system directories..."
mkdir -p \
    /run/sshd \
    /root/.ssh

chmod 700 /root/.ssh

# -------------------------------------------------
# 2. Configure SSH
# -------------------------------------------------
echo "Configuring SSH server..."
sed -i 's/#PermitRootLogin prohibit-password/PermitRootLogin yes/' /etc/ssh/sshd_config
sed -i 's/#PasswordAuthentication yes/PasswordAuthentication no/' /etc/ssh/sshd_config
sed -i 's/#PubkeyAuthentication yes/PubkeyAuthentication yes/' /etc/ssh/sshd_config

# -------------------------------------------------
# 4. Configure network interface naming
# -------------------------------------------------
echo "Configuring network interface naming..."
mkdir -p /etc/systemd/network/99-default.link.d
cat <<'EOF' > /etc/systemd/network/99-default.link.d/traditional-naming.conf
[Link]
NamePolicy=keep kernel
EOF

# -------------------------------------------------
# 5. Set root password
# -------------------------------------------------
echo "Setting root password..."
echo 'root:spinbox' | chpasswd

# -------------------------------------------------
# 6. Disable MOTD
# -------------------------------------------------
echo "Disabling MOTD..."
rm -rf /etc/update-motd.d/*
truncate -s 0 /etc/motd
sed -i 's/^session.*pam_motd.so/#&/' /etc/pam.d/sshd

echo "✅ System configuration complete"
