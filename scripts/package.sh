#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")/.."
if [[ $(uname -s) != Darwin ]]; then
  echo 'Release packaging requires macOS and a native build.' >&2
  exit 1
fi
test -x R2Publisher.lrplugin/bin/r2publisher
mkdir -p dist
task_package_dir=$(mktemp -d "$PWD/.cache/package.XXXXXX")
trap 'rm -rf "$task_package_dir"' EXIT
mkdir -p docs/licenses
(cd uploader && go list -deps -f '{{if .Module}}{{.Module.Dir}}{{end}}' ./cmd/r2publisher) | sort -u > "$task_package_dir/module-dirs.txt"
while IFS= read -r module_dir; do
  [[ -n "$module_dir" ]] || continue
  module_name=$(basename "$module_dir")
  for license_file in "$module_dir/LICENSE" "$module_dir/LICENSE.txt" "$module_dir/NOTICE" "$module_dir/NOTICE.txt"; do
    if [[ -f "$license_file" ]]; then
      license_name=$(basename "$license_file")
      install -m 644 "$license_file" "docs/licenses/$module_name-${license_name%.txt}.txt"
    fi
  done
done < "$task_package_dir/module-dirs.txt"
install -m 644 "$(go env GOROOT)/LICENSE" docs/licenses/Go-LICENSE.txt
cp -R R2Publisher.lrplugin "$task_package_dir/"
cp README.md THIRD_PARTY_NOTICES.md "$task_package_dir/"
cp -R docs schemas "$task_package_dir/"
if [[ -d site/.git ]]; then
  git -C site diff --binary > dist/photo-site-r2.patch
  # Include new source files in the reviewable patch without modifying the index.
  git -C site ls-files --others --exclude-standard -z > "$task_package_dir/new-files.txt"
  while IFS= read -r -d '' file; do
    [[ -f "site/$file" && ! -L "site/$file" ]] || { echo "Unexpected non-source file: $file" >&2; exit 1; }
    git -C site diff --no-index --binary -- /dev/null "$file" >> dist/photo-site-r2.patch || [[ $? == 1 ]]
  done < "$task_package_dir/new-files.txt"
fi
archive="$PWD/dist/R2Publisher-macos-$(uname -m).zip"
rm -f "$archive"
(cd "$task_package_dir" && zip -qr "$archive" R2Publisher.lrplugin README.md THIRD_PARTY_NOTICES.md docs schemas)
shasum -a 256 "$archive" > "$archive.sha256"
echo "$archive"
