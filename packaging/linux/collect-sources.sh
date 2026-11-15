#!/bin/sh
set -eu
mkdir -p /ucd-sources
dpkg-query -W -f='${binary:Package}\t${Version}\t${source:Package}\t${source:Version}\n' > /ucd-sources/packages.tsv
# Debian's deb822 definitions retain the archive's signed Release verification.
sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/debian.sources
apt-get update
cd /ucd-sources
dpkg-query -W -f='${source:Package}=${source:Version}\n' > source-packages.txt
# Offline repair payloads can have newer library versions than installed packages.
for archive in /usr/local/share/ucd/procps/*.deb; do
    source=$(dpkg-deb -f "$archive" Source)
    version=$(dpkg-deb -f "$archive" Version)
    case "$source" in
        *' ('*) version=${source##* (}; version=${version%)}; source=${source%% (*} ;;
        '') source=$(dpkg-deb -f "$archive" Package) ;;
    esac
    printf '%s=%s\n' "$source" "$version" >> source-packages.txt
    dpkg-deb -W --showformat='${Package}\t${Version}\t${Source}\n' "$archive" >> staged-packages.tsv
done
sort -u source-packages.txt -o source-packages.txt
while IFS= read -r package; do
    case "$package" in
        docker-ce=*|docker-ce-cli=*|containerd.io=*|docker-buildx-plugin=*) continue ;;
    esac
    # Fail if the exact source is unavailable; never substitute another version.
    apt-get source --download-only --only-source "$package"
done < source-packages.txt
cp /var/lib/dpkg/status dpkg-status
find . -type f ! -name SHA256SUMS -print0 | sort -z | xargs -0 sha256sum > SHA256SUMS
tar -czf /ucd-rootfs-sources.tar.gz .
