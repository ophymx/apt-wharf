#!/bin/sh

set -e

fix_mode="apply"
for arg in "$@"; do
    case "$arg" in
        --diff)   fix_mode="diff" ;;
        --apply)  fix_mode="apply" ;;
        -h|--help)
            echo "usage: $0 [--diff|--apply]"
            echo "  --apply  run 'go fix' in place (default)"
            echo "  --diff   show 'go fix' diffs without rewriting files"
            exit 0
            ;;
        *) echo "unknown flag: $arg" >&2; exit 2 ;;
    esac
done

set -x

go fmt ./...
go vet ./...
if [ "$fix_mode" = "diff" ]; then
    go fix -diff ./...
else
    go fix ./...
fi
go build ./...
go test ./...
go mod tidy

goreleaser check

git status
