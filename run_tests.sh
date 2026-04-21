#!/usr/bin/env bash
set -ex

dir=$(pwd)

GV=$(go version | sed "s/^.*go\([0-9.]*\).*/\1/")
echo "Go version: $GV"

# Ensure html templates pass localization.
go generate -x ./wallet/webserver/site # no -write

cd "$dir"

# list of all modules to test
modules=". /dex/testing/loadbot /wallet/cmd/bisonw-desktop"

# For each module, run go mod tidy, build and run test.
for m in $modules
do
	cd "$dir/$m"

	# Run `go mod tidy` and fail if the git status of go.mod and/or
	# go.sum changes. Only do this for the latest Go version.
	if [[ "$GV" =~ ^1.25 ]]; then
		MOD_STATUS=$(git status --porcelain go.mod go.sum)
		go mod tidy
		UPDATED_MOD_STATUS=$(git status --porcelain go.mod go.sum)
		if [ "$UPDATED_MOD_STATUS" != "$MOD_STATUS" ]; then
			echo "$m: running 'go mod tidy' modified go.mod and/or go.sum"
		git diff --unified=0 go.mod go.sum
			exit 1
		fi
	fi

	# run tests
	env GORACE="halt_on_error=1" go test -race -short ./...
done

cd "$dir"

# Print missing Core notification translations.
go run ./wallet/core/localetest/main.go

# -race in go tests above requires cgo, but disable it for the compile tests below
export CGO_ENABLED=0
go build ./...
go build -tags harness -o /dev/null ./wallet/cmd/simnet-trade-tests
go build -tags systray -o /dev/null ./wallet/cmd/bisonw

go test -c -o /dev/null -tags live ./wallet/webserver
go test -c -o /dev/null -tags harness ./wallet/asset/dcr
go test -c -o /dev/null -tags electrumlive ./wallet/asset/btc
go test -c -o /dev/null -tags harness ./wallet/asset/btc/livetest
go test -c -o /dev/null -tags harness ./wallet/asset/ltc
go test -c -o /dev/null -tags harness ./wallet/asset/bch
go test -c -o /dev/null -tags harness ./wallet/asset/firo
go test -c -o /dev/null -tags harness ./wallet/asset/eth
go test -c -o /dev/null -tags rpclive ./wallet/asset/eth
go test -c -o /dev/null -tags harness ./wallet/asset/zec
go test -c -o /dev/null -tags harness ./wallet/asset/dash
go test -c -o /dev/null -tags harness ./wallet/asset/firo
go test -c -o /dev/null -tags rpclive ./wallet/asset/polygon
go test -c -o /dev/null -tags live ./dex/testing/firo/test

# Return to initial directory.
cd "$dir"
# golangci-lint (github.com/golangci/golangci-lint) is used to run each
# static checker.

# Lint markdown files if markdownlint-cli2 is installed.
if command -v markdownlint-cli2 > /dev/null 2>&1; then
	markdownlint-cli2 "*.md" "docs/**/*.md"
fi

# check linters
golangci-lint -c ./.golangci.yml run
