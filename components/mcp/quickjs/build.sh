#!/bin/sh
# Builds ../quickjs.wasm: sandbox.c linked with the QuickJS engine of
# quickjs-ng, without quickjs-libc, with a 1 MiB stack at the bottom of
# linear memory and a 64 MiB memory maximum. See README.md.
#
# Needs git, wasm-opt (binaryen) and wasi-sdk 25 at $WASI_SDK.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
wasi_sdk=${WASI_SDK:-/opt/wasi-sdk}
out=${OUT:-$here/../quickjs.wasm}
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# The output is reproducible only with the pinned toolchain; README.md
# records the sha256 it gives.
"$wasi_sdk/bin/clang" --version | grep -q '^clang version 19\.1\.5-wasi-sdk' || {
	echo "build.sh: need wasi-sdk 25 (clang 19.1.5) at $wasi_sdk" >&2
	exit 1
}
test "$(wasm-opt --version)" = "wasm-opt version 133" || {
	echo "build.sh: need binaryen 133 (wasm-opt version 133)" >&2
	exit 1
}

git clone --quiet https://github.com/quickjs-ng/quickjs "$work/quickjs"
git -C "$work/quickjs" checkout --quiet v0.17.0
test "$(git -C "$work/quickjs" rev-parse HEAD)" = 6d46d07d04041b40f4f49eaa7fdebe44c314c699
cp "$here/sandbox.c" "$work/quickjs/sandbox.c"

# Relative paths and a fixed prefix map keep the build directory out of the
# output.
cd "$work/quickjs"
"$wasi_sdk/bin/clang" \
	--target=wasm32-wasip1 --sysroot="$wasi_sdk/share/wasi-sysroot" \
	-O2 -DNDEBUG -D_GNU_SOURCE -funsigned-char -ffile-prefix-map="$work"=. \
	-mexec-model=reactor \
	-Wl,--export=malloc -Wl,--export=free \
	-Wl,--stack-first -Wl,-z,stack-size=1048576 \
	-Wl,--initial-memory=2097152 -Wl,--max-memory=67108864 \
	-o sandbox.wasm \
	sandbox.c quickjs.c libregexp.c libunicode.c dtoa.c -lm
wasm-opt -O3 --strip-debug sandbox.wasm -o "$out"
shasum -a 256 "$out"
