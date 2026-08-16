#!/usr/bin/env bash
# End-to-end verification of every docksmith subsystem.
#
#   sudo make demo          full run (resets state first)
#   sudo ./scripts/demo.sh --keep-state
#
# Requires root: namespaces, pivot_root and netlink. Exits non-zero if any
# check fails, so it is usable in CI.

set -u -o pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO"

# Mirror cmd/stateRoot(): sudo sets HOME=/root, so trusting $HOME would point
# at a different store than the binary uses.
if [ -n "${DOCKSMITH_ROOT:-}" ]; then
    STATE="$DOCKSMITH_ROOT"
elif [ -n "${SUDO_USER:-}" ] && [ "${SUDO_USER}" != "root" ]; then
    STATE="$(getent passwd "$SUDO_USER" | cut -d: -f6)/.docksmith"
else
    STATE="$HOME/.docksmith"
fi

CTX=/tmp/docksmith-demo
VOL=/tmp/docksmith-demo-vol
ARCHIVE=/tmp/docksmith-demo.tar
PASSED=0; FAILED=0; SKIPPED=0

red()    { printf '\033[31m%s\033[0m\n' "$*"; }
green()  { printf '\033[32m%s\033[0m\n' "$*"; }
yellow() { printf '\033[33m%s\033[0m\n' "$*"; }
section(){ printf '\n\033[1m━━ %s\033[0m\n' "$*"; }
pass()   { green "  PASS  $1"; PASSED=$((PASSED+1)); }
fail()   { red   "  FAIL  $1"; [ $# -gt 1 ] && printf '        %s\n' "$2"; FAILED=$((FAILED+1)); }
skip()   { yellow "  SKIP  $1"; SKIPPED=$((SKIPPED+1)); }

# ok <desc> <cmd...>          — passes if the command succeeds
ok()     { local d="$1"; shift; "$@" >/dev/null 2>&1 && pass "$d" || fail "$d"; }
# notok <desc> <cmd...>       — passes if the command fails
notok()  { local d="$1"; shift; "$@" >/dev/null 2>&1 && fail "$d" || pass "$d"; }
# inside <desc> <net> <snippet> — snippet must echo PASS
inside() {
    local d="$1" net="$2" s="$3" out
    out="$(./docksmith run --rm --net="$net" demo:1 /bin/sh -c "$s" 2>/dev/null)"
    printf '%s' "$out" | grep -q PASS && pass "$d" || fail "$d" "got: ${out:-<empty>}"
}

[ "$(id -u)" -eq 0 ] || { red "must run as root: sudo make demo"; exit 1; }

section "Setup"
if [ "${1:-}" != "--keep-state" ]; then
    rm -rf "$STATE"
fi
./setup/import-base-images.sh >/dev/null 2>&1 || { red "base image import failed"; exit 1; }
mkdir -p "$CTX/src" "$CTX/config"
echo 'echo "app running: $GREETING"' > "$CTX/src/main.sh"
echo 'setting=1' > "$CTX/config/settings.txt"
echo 'should-not-be-copied' > "$CTX/debug.log"
printf '*.log\n' > "$CTX/.docksmithignore"
cat > "$CTX/Docksmithfile" <<'EOF'
FROM busybox:latest
ENV GREETING=Hello
WORKDIR /app
EXPOSE 8080
COPY src/main.sh /app/main.sh
COPY config/settings.txt /app/settings.txt
RUN echo built > /app/marker.txt
RUN rm /bin/ls
RUN ln -s /app/marker.txt /app/link-to-marker
RUN chmod 755 /app/main.sh
RUN rm /app/settings.txt && mkdir -p /app/settings.txt && echo nested > /app/settings.txt/inner
CMD ["/bin/sh", "/app/main.sh"]
EOF
green "  ok"

section "1. Cold build (all CACHE MISS)"
./docksmith build -t demo:1 "$CTX" | tee /tmp/demo-cold.txt
grep -q "CACHE MISS" /tmp/demo-cold.txt && pass "steps report CACHE MISS" || fail "steps report CACHE MISS"

section "2. Warm build (all CACHE HIT, stable digest)"
cold_id="$(grep -o 'sha256:[0-9a-f]*' /tmp/demo-cold.txt | tail -1)"
./docksmith build -t demo:1 "$CTX" | tee /tmp/demo-warm.txt
warm_id="$(grep -o 'sha256:[0-9a-f]*' /tmp/demo-warm.txt | tail -1)"
grep -q "CACHE HIT" /tmp/demo-warm.txt && pass "steps report CACHE HIT" || fail "steps report CACHE HIT"
[ "$cold_id" = "$warm_id" ] && pass "manifest digest stable across rebuilds" \
    || fail "manifest digest stable across rebuilds" "$cold_id vs $warm_id"

section "3. Cache invalidation on source change"
echo '# changed' >> "$CTX/config/settings.txt"
./docksmith build -t demo:1 "$CTX" | tee /tmp/demo-edit.txt
grep -q "COPY config/settings.txt.*CACHE MISS" /tmp/demo-edit.txt \
    && pass "edited COPY invalidates" || fail "edited COPY invalidates"
grep -q "RUN echo built.*CACHE MISS" /tmp/demo-edit.txt \
    && pass "cascade: later steps also miss" || fail "cascade: later steps also miss"

ok "chmod on a COPY source invalidates its cache entry" \
    sh -c 'chmod 700 "$0/src/main.sh"; ./docksmith build -t demo:1 "$0" | grep -q "COPY src/main.sh.*CACHE MISS"' "$CTX"

section "4. .docksmithignore"
inside "ignored files are not copied into the image" bridge \
    '[ -e /app/debug.log ] && echo FAILED || echo PASS'

section "5. Whiteouts (layer deletions)"
inside "a file removed by RUN stays removed" bridge \
    '[ -e /bin/ls ] && echo FAILED || echo PASS'
ok "base image is undamaged by the derived image's deletion" \
    ./docksmith run --rm busybox:latest /bin/ls /bin/ls

inside "a symlink created by RUN is still a symlink in the image" bridge \
    '[ -L /app/link-to-marker ] && echo PASS || echo FAILED'
inside "  and it still points where it was made to point" bridge \
    '[ "$(readlink /app/link-to-marker)" = /app/marker.txt ] && echo PASS || echo FAILED'
inside "a file replaced by a directory reassembles as a directory" bridge \
    '[ -d /app/settings.txt ] && [ "$(cat /app/settings.txt/inner)" = nested ] && echo PASS || echo FAILED'
inside "a chmod-only RUN survives into the image" bridge \
    '[ -x /app/main.sh ] && echo PASS || echo FAILED'

section "6. Isolation"
inside "no /.oldroot leaked (pivot_root cleaned up)" bridge \
    '[ -e /.oldroot ] && echo FAILED || echo PASS'
inside "container root is a real mount point" bridge \
    'awk "\$5 == \"/\" {f=1} END {exit !f}" /proc/self/mountinfo && echo PASS || echo FAILED'
inside "PID namespace is isolated" bridge \
    '[ "$(ls /proc | grep -c "^[0-9]*$")" -lt 20 ] && echo PASS || echo FAILED'
rm -f /tmp/docksmith-escape.txt
./docksmith run --rm demo:1 /bin/sh -c 'echo x > /tmp/docksmith-escape.txt' >/dev/null 2>&1
[ -f /tmp/docksmith-escape.txt ] && fail "container writes stay inside" || pass "container writes stay inside"
leaked="$(awk '$2 ~ /docksmith/ {n++} END {print n+0}' /proc/mounts)"
[ "$leaked" -eq 0 ] && pass "no mounts leaked to the host" \
    || fail "no mounts leaked to the host" "$leaked found"

section "7. Environment and command overrides"
out="$(./docksmith run --rm demo:1 2>/dev/null)"
printf '%s' "$out" | grep -q "Hello" && pass "image CMD and ENV apply" || fail "image CMD and ENV apply" "$out"
out="$(./docksmith run --rm -e GREETING=Howdy demo:1 2>/dev/null)"
printf '%s' "$out" | grep -q "Howdy" && pass "-e overrides ENV" || fail "-e overrides ENV" "$out"

section "8. Exit codes and the init shim"
./docksmith run --rm demo:1 /bin/sh -c 'exit 42' >/dev/null 2>&1
[ $? -eq 42 ] && pass "exit code propagates" || fail "exit code propagates"
./docksmith run -d --name demo_sig demo:1 /bin/sh -c 'sleep 60' >/dev/null 2>&1
sleep 1.5
start=$(date +%s)
./docksmith stop demo_sig >/dev/null 2>&1
elapsed=$(( $(date +%s) - start ))
# Without an init handling SIGTERM this would take the full 10s timeout.
[ "$elapsed" -lt 5 ] && pass "stop terminates via SIGTERM (${elapsed}s)" \
    || fail "stop terminates via SIGTERM" "took ${elapsed}s"
./docksmith rm demo_sig >/dev/null 2>&1

section "9. Volumes"
rm -rf "$VOL"; mkdir -p "$VOL"; echo host-file > "$VOL/from-host.txt"
./docksmith run --rm -v "$VOL:/data" demo:1 /bin/sh -c 'echo from-container > /data/out.txt' >/dev/null 2>&1
[ -f "$VOL/out.txt" ] && pass "container writes reach the host" || fail "container writes reach the host"
out="$(./docksmith run --rm -v "$VOL:/data:ro" demo:1 \
    /bin/sh -c 'echo x > /data/blocked 2>/dev/null && echo WROTE || echo PASS' 2>/dev/null)"
printf '%s' "$out" | grep -q PASS && pass ":ro mounts refuse writes" || fail ":ro mounts refuse writes" "$out"

section "10. Container lifecycle"
cid="$(./docksmith run -d --name demo_bg demo:1 /bin/sh -c 'echo started; sleep 60' 2>/dev/null)"
sleep 1.5
./docksmith ps 2>/dev/null | grep -q demo_bg && pass "detached container appears in ps" || fail "detached container appears in ps"
./docksmith logs demo_bg 2>/dev/null | grep -q started && pass "logs captured" || fail "logs captured"
ok "resolve by id prefix" ./docksmith logs "${cid:0:6}"
./docksmith stop demo_bg >/dev/null 2>&1
./docksmith ps -a 2>/dev/null | grep demo_bg | grep -q Exited && pass "exit recorded after stop" || fail "exit recorded after stop"
# Start and assert separately: folding both into one sh -c meant a failed
# `run -d` also failed the `rm`, so the check passed for the wrong reason.
ok "detached container for the rm check starts" \
    ./docksmith run -d --name demo_live demo:1 /bin/sh -c "sleep 30"
sleep 1
notok "rm refuses a running container" ./docksmith rm demo_live
ok "rm -f removes a running container" ./docksmith rm -f demo_live
ok "rm removes an exited container" ./docksmith rm demo_bg

section "11. Networking"
ok "docksmith0 bridge exists" ip link show docksmith0
inside "container gets an address in 172.30.0.0/16" bridge \
    'ip addr show eth0 2>/dev/null | grep -q "inet 172.30." && echo PASS || echo FAILED'
inside "gateway is reachable" bridge \
    'ping -c1 -W2 172.30.0.1 >/dev/null 2>&1 && echo PASS || echo FAILED'
inside "resolv.conf has no loopback resolver" bridge \
    'grep -q "nameserver 127." /etc/resolv.conf 2>/dev/null && echo FAILED || echo PASS'
if ping -c1 -W2 8.8.8.8 >/dev/null 2>&1; then
    inside "outbound NAT works" bridge 'ping -c1 -W3 8.8.8.8 >/dev/null 2>&1 && echo PASS || echo FAILED'
    inside "DNS resolution works" bridge 'nslookup example.com >/dev/null 2>&1 && echo PASS || echo FAILED'
else
    skip "outbound NAT (host has no internet)"; skip "DNS resolution (host has no internet)"
fi
inside "--net=none has no eth0" none 'ip addr show eth0 >/dev/null 2>&1 && echo FAILED || echo PASS'
inside "--net=none still has loopback" none 'ping -c1 -W1 127.0.0.1 >/dev/null 2>&1 && echo PASS || echo FAILED'
inside "builds and net=none cannot reach the gateway" none \
    'ping -c1 -W1 172.30.0.1 >/dev/null 2>&1 && echo FAILED || echo PASS'

section "12. Published ports"
./docksmith run -d --name demo_port -p 18080:8080 demo:1 /bin/sh -c \
    'while true; do echo -e "HTTP/1.1 200 OK\r\nContent-Length: 8\r\n\r\nit-works" | nc -l -p 8080 2>/dev/null || sleep 0.2; done' >/dev/null 2>&1
sleep 2
./docksmith ps 2>/dev/null | grep -q "18080->8080" && pass "ps shows the published port" || fail "ps shows the published port"
timeout 5 sh -c 'echo | nc -w2 127.0.0.1 18080' 2>/dev/null | grep -q it-works \
    && pass "reachable at 127.0.0.1" || fail "reachable at 127.0.0.1"
lan="$(ip -4 addr show scope global | grep -oP 'inet \K[\d.]+' | head -1)"
if [ -n "$lan" ]; then
    timeout 5 sh -c "echo | nc -w2 $lan 18080" 2>/dev/null | grep -q it-works \
        && pass "reachable at the LAN address" || fail "reachable at the LAN address"
else
    skip "LAN address test (no global address)"
fi
./docksmith rm -f demo_port >/dev/null 2>&1
sleep 0.5
iptables -t nat -S 2>/dev/null | grep -q "dport 18080" \
    && fail "DNAT rules removed on rm" || pass "DNAT rules removed on rm"

section "13. Builds have no network"
cat > "$CTX/netcheck" <<'EOF'
FROM busybox:latest
RUN ping -c1 -W1 8.8.8.8 >/dev/null 2>&1 && echo ONLINE > /net.txt || echo OFFLINE > /net.txt
CMD ["/bin/cat", "/net.txt"]
EOF
mkdir -p "$CTX/nc" && mv "$CTX/netcheck" "$CTX/nc/Docksmithfile"
./docksmith build --no-cache -t netcheck:1 "$CTX/nc" >/dev/null 2>&1
out="$(./docksmith run --rm netcheck:1 2>/dev/null)"
printf '%s' "$out" | grep -q OFFLINE && pass "RUN steps have no network access" \
    || fail "RUN steps have no network access" "got: $out"

section "14. save / load"
./docksmith save -o "$ARCHIVE" demo:1 >/dev/null 2>&1
[ -s "$ARCHIVE" ] && pass "archive written" || fail "archive written"
tar tf "$ARCHIVE" 2>/dev/null | grep -q '^index.json$' && pass "archive has an index" || fail "archive has an index"
DOCKSMITH_ROOT=/tmp/docksmith-demo-store ./docksmith load -i "$ARCHIVE" >/dev/null 2>&1 \
    && pass "loads into a fresh store" || fail "loads into a fresh store"
orig="$(./docksmith images 2>/dev/null | awk '$1=="demo"{print $3}')"
copy="$(DOCKSMITH_ROOT=/tmp/docksmith-demo-store ./docksmith images 2>/dev/null | awk '$1=="demo"{print $3}')"
[ -n "$orig" ] && [ "$orig" = "$copy" ] && pass "round trip preserves the digest" \
    || fail "round trip preserves the digest" "$orig vs $copy"
python3 -c "
d=bytearray(open('$ARCHIVE','rb').read()); d[len(d)//2]^=0xff
open('$ARCHIVE.bad','wb').write(d)" 2>/dev/null
notok "corrupt archive is rejected" env DOCKSMITH_ROOT=/tmp/docksmith-demo-store2 ./docksmith load -i "$ARCHIVE.bad"
rm -rf /tmp/docksmith-demo-store /tmp/docksmith-demo-store2 "$ARCHIVE" "$ARCHIVE.bad"

section "15. rmi reference counting"
ok "rmi removes a derived image" ./docksmith rmi netcheck:1
ok "sibling image still runs (shared base layer survived)" ./docksmith run --rm demo:1 /bin/true

section "16. prune"
./docksmith run --rm demo:1 /bin/true >/dev/null 2>&1
./docksmith run --name demo_exited demo:1 /bin/true >/dev/null 2>&1
out="$(./docksmith prune -f 2>&1)"
printf '%s' "$out" | grep -qE "Reclaimed|Nothing to prune" && pass "prune reports what it reclaimed" \
    || fail "prune reports what it reclaimed" "$out"
ok "images still usable after prune" ./docksmith run --rm demo:1 /bin/true

section "17. Unit tests"
go test ./... >/dev/null 2>&1 && pass "go test ./..." || { fail "go test ./..."; go test ./...; }

section "Cleanup"
./docksmith ps -a 2>/dev/null | tail -n +2 | awk '{print $1}' | xargs -r ./docksmith rm -f >/dev/null 2>&1
rm -rf "$CTX" "$VOL" /tmp/demo-*.txt /tmp/docksmith-escape.txt
green "  ok"

section "Result"
[ "$SKIPPED" -gt 0 ] && yellow "$SKIPPED check(s) skipped"
if [ "$FAILED" -eq 0 ]; then
    green "ALL $PASSED CHECKS PASSED"
    exit 0
fi
red "$FAILED of $((PASSED+FAILED)) checks FAILED"
exit 1
