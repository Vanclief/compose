# quickjs.wasm

`../quickjs.wasm` is `sandbox.c` linked with the QuickJS engine of quickjs-ng,
compiled to WebAssembly (WASI preview 1, reactor). `sandbox.c` documents the
protocol between the guest and `../script.go`: one `run` export plus wasi-libc's
`malloc` and `free`, and two imports from module `env`, `host_call` and
`host_log`. Strings cross with their length.

What it leaves out and how it is laid out:

- Only the engine: `quickjs.c`, `libregexp.c`, `libunicode.c` and `dtoa.c`.
  `quickjs-libc.c` is not linked, so there is no `qjs:std`, `qjs:os` or
  `qjs:bjson` module, no `print`, `scriptArgs` or `gc` global, and no module
  loader: every `import()` fails.
- `-Wl,--stack-first -Wl,-z,stack-size=1048576`: a 1 MiB stack at the bottom
  of linear memory. Since quickjs-ng v0.17.0 (commit a2563ad, "Add WASI
  configurable stack overflow protection") `JS_SetMaxStackSize` works under
  WASI, so `script.go` passes a 960 KiB limit and deep recursion throws a
  catchable `RangeError`. Native code that recurses past the last check runs
  the stack pointer below address 0 and traps instead of overwriting static
  data.
- `-Wl,--max-memory=67108864 -Wl,--initial-memory=2097152`: 1024 pages
  (64 MiB) maximum, 32 pages initial. `script.go` sets the QuickJS heap limit
  to 48 MiB, so an allocation bomb ends in `InternalError: out of memory` while
  wasi-libc's allocator still has room for function call replies. Since
  quickjs-ng v0.18.0 (commits 90aa2c4 and db0fe52) an InternalError, out of
  memory included, skips the script's `catch` and `finally` blocks, except
  where QuickJS's Promise code, such as a Promise executor, turns it into a
  rejection.

## Inputs

| | |
|---|---|
| quickjs-ng | `d7215b0ad3fc2c562e8317fd9bca058fd4863804` (v0.18.0) |
| wasi-sdk | 25.0, `wasi-sdk-25.0-arm64-macos.tar.gz` sha256 `e1e529ea226b1db0b430327809deae9246b580fa3cae32d31c82dfe770233587` (clang 19.1.5) |
| binaryen | wasm-opt version 133 |

## Flags

| | |
|---|---|
| clang | `--target=wasm32-wasip1 --sysroot=$WASI_SDK/share/wasi-sysroot -O2 -DNDEBUG -D_GNU_SOURCE -funsigned-char -ffile-prefix-map=<build dir>=. -mexec-model=reactor` |
| linker | `--export=malloc --export=free --stack-first -z stack-size=1048576 --initial-memory=2097152 --max-memory=67108864` |
| wasm-opt | `-O3 --strip-debug` |

## Build

```sh
WASI_SDK=/path/to/wasi-sdk-25.0 ./build.sh
```

It checks the clang and wasm-opt versions, clones quickjs-ng, checks the
commit of the tag, compiles the four engine files with `sandbox.c` directly
with clang (no CMake), runs `wasm-opt` and writes `../quickjs.wasm`. The output
is reproducible across build directories:

```
8ea590fe0ea0f422858800fcf81acf1ecef8fdf21aa6d55946b00b7c0a4860af  quickjs.wasm
```

`TestScriptWasmMemoryIsCapped` pins this sha256, the memory maximum and the
stack placement. Moving to another wasi-sdk or binaryen changes the hash: update
the versions in `build.sh` and this file, rebuild, check the layout assertions
of the test still pass, then update the pinned hash.

### Verified builds

Only the macOS arm64 build, from the `wasi-sdk-25.0-arm64-macos.tar.gz`
tarball above, has been verified to produce the pinned sha256. A Linux build
with wasi-sdk 25 is expected to match, since it uses the same clang 19.1.5
and binaryen 133, `-ffile-prefix-map` and no timestamps, but it has not been
checked. Whoever first builds on Linux should record the wasi-sdk tarball and
its sha256 here, with the hash of the `quickjs.wasm` it produced.
