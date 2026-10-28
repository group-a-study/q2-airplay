#!/bin/sh
# cliairplay (Music Assistant's AirPlay 1 and 2 sender, github.com/music-assistant/airplay-cli) for the
# Shanling Q2: MIPS32r2, FP64 and NaN-2008 hard float, dynamically linked against the Q2's own glibc
# 2.28, libstdc++ (GCC 10) and libcrypto 1.1 from its firmware's rootfs, as Q2 Pod's helpers are.
#   contrib/shanlingq2/build-cliairplay.sh FIRMWARE_ZIP OUT
# OUT/cliairplay is the binary. Needs clang, lld, llvm-ar and llvm-strip (18 or later; LLVM_SUFFIX=-18
# where they carry their version), git, curl, unsquashfs and python3. FIRMWARE_ZIP is Shanling's
# "Q2 Firmware V1.32.zip" (SHA-256 154c1782…0f00, the one Q2 Pod audits).
#
# The sources are fetched by commit: airplay-cli and the parts of its libraop submodule it builds
# (libraop's RAOP client, crosstools, dmap-parser, the ALAC encoder, mdnssd); libraop's prebuilt
# libraries aren't used (their MIPS ones are FP32, legacy NaN): OpenSSL is the Q2's own. One fix is
# applied: libraop joined RTP threads it never started when a connect failed early, which crashed
# AirPlay 1's fallback (upstream's Makefile notes the same under its sanitizers).
set -eu
[ $# -ge 2 ] || { echo "usage: $0 FIRMWARE_ZIP OUT" >&2; exit 2; }
ZIP=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
HERE=$(cd "$(dirname "$0")" && pwd)
mkdir -p "$2"
OUT=$(cd "$2" && pwd)
LLVM=${LLVM_SUFFIX:-}
ZIP_SHA=154c17822d09be001be35c03d2d3488424dee195221790bd70864480d55b0f00
VERSION=0.5.9-q2 # cliairplay's own version, then this build's
W=$OUT/work DL=$OUT/work/dl SRC=$OUT/work/src SYSROOT=$OUT/work/sysroot BIN=$OUT/work/bin
mkdir -p "$DL" "$SRC" "$BIN"

for t in "clang$LLVM" "ld.lld$LLVM" "llvm-ar$LLVM" "llvm-strip$LLVM" git curl unsquashfs python3; do
    command -v "$t" >/dev/null || { echo "$0: $t not found" >&2; exit 1; }
done

fetch() { # name SHA-256 URL
    f=$DL/$1
    if [ ! -e "$f" ] || [ "$(sha256sum "$f" | cut -d' ' -f1)" != "$2" ]; then
        curl -fsSL --retry 3 -o "$f.part" "$3"
        [ "$(sha256sum "$f.part" | cut -d' ' -f1)" = "$2" ] || { echo "$0: $1: SHA-256 differs" >&2; exit 1; }
        mv "$f.part" "$f"
    fi
}
checkout() { # dir URL commit
    d=$SRC/$1
    if [ "$(git -C "$d" rev-parse HEAD 2>/dev/null)" != "$3" ]; then
        rm -rf "$d"
        git init -q "$d"
        git -C "$d" fetch -q --depth 1 "$2" "$3"
        git -C "$d" checkout -q FETCH_HEAD
    fi
}
DEB=https://archive.debian.org/debian/pool/main
fetch libc6-dev-mipsel-cross_2.28-7cross1_all.deb 4e6a62a137c28ea99eecc69d766bac17a66165de731b2ed6150f3536a0120bec $DEB/c/cross-toolchain-base/libc6-dev-mipsel-cross_2.28-7cross1_all.deb
fetch libc6-mipsel-cross_2.28-7cross1_all.deb 7bca61e6becfd46e64595c011d830ce1e4e5c766e8df9f079791f1e91015f70b $DEB/c/cross-toolchain-base/libc6-mipsel-cross_2.28-7cross1_all.deb
fetch linux-libc-dev-mipsel-cross_4.19.20-1cross1_all.deb 280076fac26cce7d6375bf48f0e627fc1f6bcd58acbdb95f6ae55357dbad935a $DEB/c/cross-toolchain-base/linux-libc-dev-mipsel-cross_4.19.20-1cross1_all.deb
fetch libstdc++-10-dev-mipsel-cross_10.2.1-6cross1_all.deb 91d70efff8d84a169dec1b2443f34cfd59759367bb6a0fd7fd19bc393ac41cea $DEB/g/gcc-10-cross-mipsen/libstdc++-10-dev-mipsel-cross_10.2.1-6cross1_all.deb
fetch libssl-dev_1.1.1w-0+deb11u1_mipsel.deb ed5b7c2a182d65b5472b715fda73838b0f9bcd66d86184e1e3b387151b690822 $DEB/o/openssl/libssl-dev_1.1.1w-0+deb11u1_mipsel.deb
fetch alac_wrapper.h 31a5465e319589929dd8b0164a0e04e0a4f89cc8f92f80442729a025733be902 https://raw.githubusercontent.com/philippe44/libcodecs/3234c96680fbe9f45047936a14df10edca822e5e/addons/alac_wrapper.h
checkout airplay-cli https://github.com/music-assistant/airplay-cli 0a58c239d5fb3cf8927b9707a71c6fe897266b6a
checkout libraop https://github.com/philippe44/libraop 81c2182649da8645ac2a58b78e9f370c79a4165b
checkout crosstools https://github.com/philippe44/crosstools 41544c653760a205cbf6cebfdeda4a9394cc6455
checkout dmap-parser https://github.com/philippe44/dmap-parser 2f8ee61e2427a177beea919d72dde8931084fe51
checkout alac https://github.com/philippe44/alac 30e526563714325ff384968e116060c4ed606f06
checkout mdnssd https://github.com/philippe44/mdnssd 16402f3293b8bf09b8274ba7188202953d8e4f40

# The sysroot: Debian's glibc 2.28, libstdc++ 10 and OpenSSL 1.1 headers, the Q2's /lib and /usr/lib.
if [ ! -e "$SYSROOT/usr/lib/libc.so" ]; then
    python3 -I - "$ZIP" "$ZIP_SHA" "$W/rootfs.squashfs" "$W/deb" "$DL" <<'EOF'
import hashlib, io, pathlib, sys, tarfile, zipfile
zip_path, want, out, deb_out, dl = sys.argv[1:]
raw = open(zip_path, 'rb').read()
if hashlib.sha256(raw).hexdigest() != want:
    sys.exit(f'{zip_path}: not the Q2 V1.32 firmware ZIP (SHA-256 differs)')
with zipfile.ZipFile(io.BytesIO(raw)) as z:
    tar = z.read('Q2 Firmware V1.32/update.tar')
with tarfile.open(fileobj=io.BytesIO(tar)) as t:
    open(out, 'wb').write(t.extractfile('recovery-update/rootfs.squashfs').read())
def safe(member, path):  # pinned packages; a link out of the tree (libstdc++'s to /usr) is left out
    try:
        return tarfile.data_filter(member, path)
    except tarfile.FilterError:
        return None
for deb in sorted(pathlib.Path(dl).glob('*.deb')):  # each an ar archive: its data.tar.*
    b = deb.read_bytes()
    i = 8
    while i < len(b):
        name, size = b[i:i + 16].decode().strip().rstrip('/'), int(b[i + 48:i + 58])
        if name.startswith('data.tar'):
            with tarfile.open(fileobj=io.BytesIO(b[i + 60:i + 60 + size])) as t:
                t.extractall(deb_out, filter=safe)
        i += 60 + size + (size & 1)
EOF
    rm -rf "$W/rootfs"
    unsquashfs -no-xattrs -f -d "$W/rootfs" "$W/rootfs.squashfs" lib usr/lib >/dev/null
    rm "$W/rootfs.squashfs"
    LLVM_SUFFIX=$LLVM python3 -I "$HERE/mksysroot.py" "$W/deb" "$W/rootfs" "$SYSROOT"
fi

# The compilers: the Q2's ABI against the sysroot, libstdc++ 10's headers for C++
CXXINC="-nostdinc++ -isystem $SYSROOT/usr/include/c++/10 -isystem $SYSROOT/usr/include/c++/10/mipsel-linux-gnu"
for c in cc c++; do
    driver="clang$LLVM"
    extra=
    if [ $c = c++ ]; then driver="clang$LLVM --driver-mode=g++"; extra=$CXXINC; fi
    cat > "$BIN/q2-$c" <<EOF
#!/bin/sh
exec $driver --target=mipsel-linux-gnu -march=mips32r2 -mfp64 -mnan=2008 -mabs=2008 $extra \\
  --sysroot="$SYSROOT" -fuse-ld=lld -Wl,--dynamic-linker=/lib/ld-linux-mipsn8.so.1 -Wl,-z,noexecstack \\
  -Wno-unused-command-line-argument "\$@"
EOF
    chmod +x "$BIN/q2-$c"
done

# The sources to build, laid out as airplay-cli's tree (its sources include libraop's by relative path),
# a copy so the fix never touches a checkout
B=$W/build
L=$B/libraop
rm -rf "$B"
mkdir -p "$B/obj" "$L/libcodecs/addons" "$L/libmdns"
cp -R "$SRC/airplay-cli/src" "$B/src"
cp -R "$SRC/libraop/src" "$L/src"
cp -R "$SRC/crosstools" "$L/crosstools"
cp -R "$SRC/dmap-parser" "$L/dmap-parser"
cp -R "$SRC/mdnssd" "$L/libmdns/mdnssd"
cp -R "$SRC/alac" "$L/libcodecs/alac"
cp "$DL/alac_wrapper.h" "$L/libcodecs/addons/"
python3 -I - "$L/src/raop_client.c" <<'EOF2'
import sys
p = sys.argv[1]
t = open(p).read()
for name in ('ctrl_thread', 'time_thread'):
    old = f'\tpthread_join(p->{name}, NULL);\n'
    if t.count(old) != 1:
        sys.exit(f'{p}: the {name} join to guard is not there')
    t = t.replace(old, f'\tif (p->{name}) pthread_join(p->{name}, NULL);\n\tp->{name} = 0;\n')
open(p, 'w').write(t)
EOF2
INC="-I$L/crosstools/src -I$L/dmap-parser -I$L/libmdns/mdnssd -I$L/libcodecs/addons -I$L/libcodecs/alac/codec -I$L/src -I$L/src/inc -I$B/src"
DEF="-DNDEBUG -D_GNU_SOURCE -DOPENSSL_SUPPRESS_DEPRECATED -DSSL_STATIC_LIB"
CFLAGS="-O2 -g0 -fdata-sections -ffunction-sections -Wall -Wno-unused -Wno-multichar"
compile() { # source [flags]: one object
    s=$1
    shift
    o=$B/obj/$(basename "${s%.*}").o
    case $s in
        *.cpp) "$BIN/q2-c++" -std=gnu++17 $CFLAGS $DEF $INC "$@" -c "$s" -o "$o" ;;
        *) "$BIN/q2-cc" $CFLAGS $DEF $INC "$@" -c "$s" -o "$o" ;;
    esac
}
for s in raop_client.c rtsp_client.c aes.c aes_ctr.c alac.c pairing.cpp bplist.cpp; do compile "$L/src/$s"; done
for s in ap2_client.c ap2_hap.c ap2_io.c ap2_ptp.c ap2_ptp_peers.c ap2_ptp_shm.c ap2_plist.c ap2_mrp.c \
         ap2_session.c host_clock.c raop_session.c artwork.c announce.c ap2_bplist.cpp alac_ext.cpp; do
    compile "$B/src/$s"
done
compile "$B/src/cliairplay.c" "-DCLIAIRPLAY_VERSION=\"$VERSION\""
for s in cross_log.c cross_ssl.c cross_net.c platform.c; do compile "$L/crosstools/src/$s"; done
compile "$L/crosstools/src/cross_util.c" -Dgettime_us=crosstools_gettime_us # host_clock.c's serves libraop
compile "$L/dmap-parser/dmap_parser.c"
compile "$L/libmdns/mdnssd/mdnssd.c"
for s in ALACEncoder.cpp ALACBitUtilities.c ag_enc.c ag_dec.c dp_enc.c matrix_enc.c EndianPortable.c; do
    compile "$L/libcodecs/alac/codec/$s"
done
"$BIN/q2-c++" -O2 -Wl,--gc-sections "$B"/obj/*.o -lcrypto -lpthread -ldl -lm -lrt -latomic -o "$B/cliairplay"
"llvm-strip$LLVM" -o "$OUT/cliairplay" "$B/cliairplay"
echo "$OUT/cliairplay"
