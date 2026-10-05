#!/usr/bin/env bash
set -euo pipefail
if [[ $(uname -s) != Linux || $(uname -m) != aarch64 ]]; then
  echo 'Build natively on Linux ARM64; CI uses Debian bookworm for glibc compatibility.' >&2; exit 1
fi
release_version=${1:?Usage: scripts/build-release.sh v1.0.0}
if [[ ! $release_version =~ ^v?[0-9]+\.[0-9]+\.[0-9]+$ ]]; then exit 1; fi
task_root=$(cd -- "$(dirname -- "$0")/.." && pwd)
cd "$task_root"
build_root=$(mktemp -d)
trap 'rm -rf "$build_root"' EXIT
package_dir="$build_root/package"
mkdir -p "$package_dir/bin" "$package_dir/LICENSES" dist
curl -fsSL --retry 3 https://github.com/bluenviron/mediamtx/releases/download/v1.21.1/mediamtx_v1.21.1_linux_arm64.tar.gz -o "$build_root/mediamtx.tar.gz"
printf '%s  %s\n' 6a3aa635fb60ea9b8d566ec306f0a42ff1b6b52a3942bc2baffbe55880d4c3dd "$build_root/mediamtx.tar.gz" | sha256sum -c -
mkdir "$build_root/mediamtx"
tar -xzf "$build_root/mediamtx.tar.gz" -C "$build_root/mediamtx"
install -m 0755 "$build_root/mediamtx/mediamtx" "$package_dir/bin/mediamtx"
cp "$build_root/mediamtx/LICENSE" "$package_dir/LICENSES/MediaMTX.txt"
curl -fsSL --retry 3 https://ffmpeg.org/releases/ffmpeg-8.0.1.tar.xz -o "$build_root/ffmpeg.tar.xz"
printf '%s  %s\n' 05ee0b03119b45c0bdb4df654b96802e909e0a752f72e4fe3794f487229e5a41 "$build_root/ffmpeg.tar.xz" | sha256sum -c -
tar -xJf "$build_root/ffmpeg.tar.xz" -C "$build_root"
(
  cd "$build_root/ffmpeg-8.0.1"
  ./configure --prefix="$package_dir" --disable-doc --disable-debug --disable-ffplay --disable-autodetect
  make -j"$(nproc)"
  install -m 0755 ffmpeg ffprobe "$package_dir/bin/"
  cp COPYING.LGPLv2.1 "$package_dir/LICENSES/FFmpeg-LGPL-2.1.txt"
)
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w -X main.version=$release_version" -o "$package_dir/bin/dozor" ./cmd/dozor
printf '%s\n' "$release_version" > "$package_dir/VERSION"
printf '%s\n' 'FFmpeg 8.0.1 source: https://ffmpeg.org/releases/ffmpeg-8.0.1.tar.xz' 'Build flags: --disable-doc --disable-debug --disable-ffplay --disable-autodetect' > "$package_dir/LICENSES/SOURCES.txt"
go version -m "$package_dir/bin/dozor" > "$package_dir/LICENSES/go-modules.txt"
cp "$(go env GOROOT)/LICENSE" "$package_dir/LICENSES/Go.txt"
python3 scripts/collect-licenses.py "$package_dir/LICENSES"
tar -czf "dist/dozor_${release_version}_linux_arm64.tar.gz" -C "$package_dir" VERSION bin LICENSES
cp "$build_root/ffmpeg.tar.xz" dist/ffmpeg-8.0.1.tar.xz
echo "Created dist/dozor_${release_version}_linux_arm64.tar.gz"
