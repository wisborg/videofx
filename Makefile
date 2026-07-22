# GoCV links against OpenCV via cgo/pkg-config. Homebrew's `opencv` formula
# is now 5.x, which GoCV does not support, so we depend on the keg-only
# `opencv@4` (4.14.0) and point pkg-config at it explicitly. Without this
# every go build/test/run in this project fails to find opencv4.
#
#   brew install opencv@4
#
export PKG_CONFIG_PATH := /opt/homebrew/opt/opencv@4/lib/pkgconfig:$(PKG_CONFIG_PATH)
export CGO_CXXFLAGS := --std=c++11

.PHONY: build test run vet clean check-deps

build:
	go build -o videofx .

test:
	go test ./...

vet:
	go vet ./...

# check-deps verifies the native toolchain before you waste time on a build.
check-deps:
	@pkg-config --modversion opencv4 >/dev/null 2>&1 \
		&& echo "OK   opencv4 $$(pkg-config --modversion opencv4)" \
		|| { echo "FAIL opencv4 not found; run: brew install opencv@4"; exit 1; }
	@command -v ffmpeg >/dev/null 2>&1 \
		&& echo "OK   ffmpeg $$(ffmpeg -version 2>/dev/null | head -1 | cut -d' ' -f3)" \
		|| { echo "FAIL ffmpeg not on PATH"; exit 1; }

clean:
	rm -f videofx
