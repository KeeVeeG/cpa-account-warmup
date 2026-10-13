#!/usr/bin/env bash
set -euo pipefail

repository_root="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
output_directory="${1:-$repository_root/dist}"

if [[ $# -gt 1 ]]; then
    printf 'Usage: %s [output-directory]\n' "$0" >&2
    exit 2
fi

version="$(sed -nE 's/^const Version = "([0-9]+\.[0-9]+\.[0-9]+)"[[:space:]]*$/\1/p' "$repository_root/internal/plugin/server.go")"
if [[ ! "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    printf 'Could not read the plugin version.\n' >&2
    exit 1
fi
if [[ -n "${PLUGIN_RELEASE_TAG:-}" && "$PLUGIN_RELEASE_TAG" != "v$version" ]]; then
    printf 'Release tag must match the plugin version: v%s.\n' "$version" >&2
    exit 1
fi
repository_url="${PLUGIN_REPOSITORY_URL:-}"
if [[ -n "$repository_url" && ! "$repository_url" =~ ^https://github\.com/[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$ ]]; then
    printf 'PLUGIN_REPOSITORY_URL must be https://github.com/owner/repository.\n' >&2
    exit 1
fi

command -v go >/dev/null || { printf 'Go is required.\n' >&2; exit 1; }
command -v zip >/dev/null || { printf 'zip is required for packaging.\n' >&2; exit 1; }
if command -v sha256sum >/dev/null; then
    checksum_command=(sha256sum)
elif command -v shasum >/dev/null; then
    checksum_command=(shasum -a 256)
else
    printf 'sha256sum or shasum is required.\n' >&2
    exit 1
fi

mkdir -p -- "$output_directory"
output_directory="$(cd -- "$output_directory" && pwd)"
cd -- "$repository_root"
target_os="$(go env GOOS)"
target_arch="$(go env GOARCH)"
case "$target_os" in
    linux) extension=so ;;
    darwin) extension=dylib ;;
    windows) extension=dll ;;
    *) printf 'Unsupported target operating system: %s\n' "$target_os" >&2; exit 1 ;;
esac

platform_directory="$output_directory/$target_os-$target_arch"
library_name="cpa-account-warmup.$extension"
archive_path="$output_directory/cpa-account-warmup_${version}_${target_os}_${target_arch}.zip"
mkdir -p -- "$platform_directory"
build_arguments=(-trimpath -buildmode=c-shared)
if [[ -n "$repository_url" ]]; then
    build_arguments+=(-ldflags "-X=cpa-account-warmup/internal/plugin.RepositoryURL=$repository_url")
fi
CGO_ENABLED=1 go build "${build_arguments[@]}" -o "$platform_directory/$library_name" ./cmd/cpa-account-warmup
(
    cd -- "$platform_directory"
    "${checksum_command[@]}" "$library_name" > checksums.txt
)

archive_files=("$platform_directory/$library_name" "$platform_directory/checksums.txt")
for document in README.md LICENSE; do
    if [[ -f "$repository_root/$document" ]]; then
        archive_files+=("$repository_root/$document")
    fi
done
# Replacing the archive avoids keeping stale entries from a previous build.
rm -f -- "$archive_path"
zip -q -j "$archive_path" "${archive_files[@]}"
(
    cd -- "$output_directory"
    "${checksum_command[@]}" cpa-account-warmup_"${version}"_*.zip > checksums.txt
)
printf 'Built %s\nPackaged %s\n' "$platform_directory/$library_name" "$archive_path"
