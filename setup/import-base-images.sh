#!/bin/bash
# Docksmith base image setup script.
# Run this once before any builds. Creates the busybox:latest base image.

set -e
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DOCKSMITH="${SCRIPT_DIR}/../docksmith"

if [ ! -f "$DOCKSMITH" ]; then
    echo "ERROR: docksmith binary not found at $DOCKSMITH"
    echo "Please run 'make build' first."
    exit 1
fi

echo "==> Creating busybox:latest base image..."

ROOTFS="$(mktemp -d)"
trap "rm -rf $ROOTFS" EXIT

# Find busybox static binary
BUSYBOX_BIN=""
for candidate in /usr/bin/busybox /bin/busybox; do
    if [ -f "$candidate" ]; then
        BUSYBOX_BIN="$candidate"
        break
    fi
done

if [ -z "$BUSYBOX_BIN" ]; then
    echo "ERROR: busybox-static not found."
    echo "Install it with: sudo apt-get install -y busybox-static"
    exit 1
fi

# Build minimal rootfs
mkdir -p "$ROOTFS/bin" "$ROOTFS/sbin" "$ROOTFS/usr/bin" "$ROOTFS/usr/sbin"
mkdir -p "$ROOTFS/lib" "$ROOTFS/lib64" "$ROOTFS/etc" "$ROOTFS/proc"
mkdir -p "$ROOTFS/dev" "$ROOTFS/tmp" "$ROOTFS/var/tmp" "$ROOTFS/home" "$ROOTFS/root"

cp "$BUSYBOX_BIN" "$ROOTFS/bin/busybox"
chmod 755 "$ROOTFS/bin/busybox"

# Create symlinks for common commands
for cmd in sh ash ls cat echo cp mv rm mkdir rmdir pwd grep sed awk find \
           head tail wc sort uniq tr cut date printf test env sleep which \
           chmod chown stat touch ln kill true false basename dirname expr; do
    ln -sf busybox "$ROOTFS/bin/$cmd"
done

# /usr/bin symlinks
for cmd in awk grep sed find sort uniq wc tr cut head tail env; do
    ln -sf /bin/busybox "$ROOTFS/usr/bin/$cmd"
done

cat > "$ROOTFS/etc/passwd" << 'EOF'
root:x:0:0:root:/root:/bin/sh
EOF
cat > "$ROOTFS/etc/group" << 'EOF'
root:x:0:root
EOF

echo "==> Importing rootfs as busybox:latest..."
"$DOCKSMITH" import "$ROOTFS" busybox:latest

echo ""
echo "==> Base images available:"
"$DOCKSMITH" images
echo ""
echo "Setup complete! You can now run 'docksmith build'."
