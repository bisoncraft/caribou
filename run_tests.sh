#!/usr/bin/env bash
set -ex

dir=$(pwd)

GV=$(go version | sed "s/^.*go\([0-9.]*\).*/\1/")
echo "Go version: $GV"

# Ensure html templates pass localization.
go generate -x ./wallet/appserver/site # no -write

cd "$dir"

# list of all modules to test
modules=". /wallet/cmd/bossw-desktop"

# For each module, run go mod tidy, build and run test.
for m in $modules
do
	cd "$dir/$m"

	# Run `go mod tidy` and fail if the git status of go.mod and/or
	# go.sum changes. Gated by RUN_TIDY_CHECK so the gate doesn't
	# silently disappear when the CI Go matrix bumps - CI sets this on
	# one matrix leg (the latest Go version) and devs can opt in
	# locally with `RUN_TIDY_CHECK=1 ./run_tests.sh`. Unset means skip.
	if [ -n "$RUN_TIDY_CHECK" ]; then
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

# The mobile package is gated by the 'mobile' build tag (it is the gomobile bind
# target), so the ./... run above skips it. Run its tests explicitly. Its logic
# is host-portable; the instrumented Android tests that need a device/emulator
# are documented in wallet/android/README.md and run separately.
env GORACE="halt_on_error=1" go test -race -short -tags mobile ./wallet/mobile/

# Print missing Core notification translations.
go run ./wallet/core/localetest/main.go

# -race in go tests above requires cgo, but disable it for the compile tests below
export CGO_ENABLED=0
go build ./...
go build -tags systray -o /dev/null ./wallet/cmd/bossw

go test -c -o /dev/null -tags live ./wallet/appserver
go test -c -o /dev/null -tags harness ./wallet/asset/dcr
go test -c -o /dev/null -tags electrumlive ./wallet/asset/btc
go test -c -o /dev/null -tags harness ./wallet/asset/ltc
go test -c -o /dev/null -tags harness ./wallet/asset/bch
go test -c -o /dev/null -tags harness ./wallet/asset/firo
go test -c -o /dev/null -tags harness ./wallet/asset/eth
go test -c -o /dev/null -tags rpclive ./wallet/asset/eth
go test -c -o /dev/null -tags harness ./wallet/asset/zec
go test -c -o /dev/null -tags harness ./wallet/asset/dash
go test -c -o /dev/null -tags rpclive ./wallet/asset/polygon

# Return to initial directory.
cd "$dir"
# golangci-lint (github.com/golangci/golangci-lint) is used to run each
# static checker.

# Lint markdown files if markdownlint-cli2 is installed.
if command -v markdownlint-cli2 > /dev/null 2>&1; then
	markdownlint-cli2 "*.md" "docs/**/*.md"
fi

# check linters. SKIP_GO_LINT lets CI skip golangci-lint on Go versions the
# pinned linter binary was not built for: a golangci-lint built with go1.25
# panics on go1.26 std-lib files. Linting is Go-version-agnostic, so running it
# on a single matrix entry is enough. Unset means run.
#
# TODO: remove SKIP_GO_LINT (and the matrix env in build.yml) once the pinned
# golangci-lint is bumped to a release built with go1.26.
if [ -n "$SKIP_GO_LINT" ]; then
	echo "Skipping golangci-lint (SKIP_GO_LINT set)."
else
	golangci-lint -c ./.golangci.yml run
fi
