#!/usr/bin/env bash
set -euo pipefail

VERSION="1.0.0"
OUTPUT_DIR="dist"

PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
)

rm -rf "${OUTPUT_DIR}"
mkdir -p "${OUTPUT_DIR}"

echo "Building Aestrl release v${VERSION}..."

for PLATFORM in "${PLATFORMS[@]}"; do
  GOOS="${PLATFORM%/*}"
  GOARCH="${PLATFORM#*/}"
  
  BINARY_NAME="aestrl"
  if [ "${GOOS}" = "windows" ]; then
    BINARY_NAME="aestrl.exe"
  fi
  
  TARGET_NAME="aestrl-${VERSION}-${GOOS}-${GOARCH}"
  TARGET_PATH="${OUTPUT_DIR}/${BINARY_NAME}"
  ARCHIVE_PATH="${OUTPUT_DIR}/${TARGET_NAME}.tar.gz"
  
  echo "  -> Compiling ${GOOS}/${GOARCH}..."
  GOOS="${GOOS}" GOARCH="${GOARCH}" CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=${VERSION}" -o "${TARGET_PATH}" ./cmd/aestrl
  
  if [ "${GOOS}" = "windows" ]; then
    ARCHIVE_PATH="${OUTPUT_DIR}/${TARGET_NAME}.zip"
    zip -q -j "${ARCHIVE_PATH}" "${TARGET_PATH}"
  else
    tar -czf "${ARCHIVE_PATH}" -C "${OUTPUT_DIR}" "${BINARY_NAME}"
  fi
  
  rm -f "${TARGET_PATH}"
  echo "     Created ${ARCHIVE_PATH}"
done

echo ""
echo "Generating SHA256 checksums..."
(
  cd "${OUTPUT_DIR}"
  sha256sum aestrl-* > checksums.txt
)

echo "Release build completed in ${OUTPUT_DIR}/"
ls -lh "${OUTPUT_DIR}"
