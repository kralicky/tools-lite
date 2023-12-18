#!/bin/bash

set -e

# sync all the files found in sync.list here. sync.list is a list of directories

TOOLS_FORK_DIR=$HOME/tools-fork

rsync -av --delete-excluded --itemize-changes \
  --exclude='*/testdata' \
  --exclude='*/*_test.go' \
  --include-from=sync.list \
  --exclude='*' \
  "$TOOLS_FORK_DIR"/ ./staging/

# in staging, replace all instances of '"golang.org/x/tools/' with '"github.com/kralicky/tools-lite"
find ./staging/ -type f -exec sed -i 's/"golang.org\/x\/tools\//\"github.com\/kralicky\/tools-lite\//g' {} \;

rm -rf ./gopls ./pkg ./go
mv ./staging/gopls .
mv ./staging/pkg .
mv ./staging/go .
rmdir ./staging

git apply .patches/*.patch

go mod tidy
