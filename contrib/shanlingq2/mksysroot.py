#!/usr/bin/env python3
"""A clang sysroot for the Shanling Q2: glibc 2.28's headers and crt objects from Debian buster's
mipsel cross packages (with libstdc++ 10's and OpenSSL 1.1's headers when their packages are there),
and the Q2's own /lib and /usr/lib (from its firmware's rootfs) to link against, so a binary binds
only symbols the Q2 has.

    mksysroot.py DEBIAN_EXTRACT Q2_ROOTFS OUT

Debian's objects are legacy NaN; they hold no float code, so their ELF flag is set to NaN-2008, the
Q2's ABI, which lld otherwise refuses to mix. glibc's linker scripts are written for the Q2's
library names, and GCC's crtbegin and crtend are empty: constructors run from .init_array."""
import os, pathlib, shutil, struct, subprocess, sys

LLVM = os.environ.get('LLVM_SUFFIX', '')
EF_MIPS_NAN2008 = 0x400


def nan2008(path):
    b = bytearray(path.read_bytes())
    if b[:4] != b'\x7fELF':
        return
    flags, = struct.unpack_from('<I', b, 0x24)
    struct.pack_into('<I', b, 0x24, flags | EF_MIPS_NAN2008)
    path.write_bytes(b)


def main():
    deb, rootfs, out = map(pathlib.Path, sys.argv[1:4])
    gnu = deb/'usr/mipsel-linux-gnu'
    if out.exists():
        shutil.rmtree(out)
    (out/'usr').mkdir(parents=True)
    shutil.copytree(gnu/'include', out/'usr/include', symlinks=True)
    # (libstdc++ 10's headers came with it, under c++/10: the Q2's libstdc++.so.6.0.28 is GCC 10's.)
    # OpenSSL 1.1's, for its libcrypto.so.1.1, from Debian's mipsel libssl-dev:
    ssl = deb/'usr/include/openssl'
    if ssl.exists():
        shutil.copytree(ssl, out/'usr/include/openssl', symlinks=True)
        shutil.copy(deb/'usr/include/mipsel-linux-gnu/openssl/opensslconf.h', out/'usr/include/openssl/')
    # glibc's headers ask for the NaN-2008 stubs list; the legacy-NaN build lists the same functions.
    shutil.copy(out/'usr/include/gnu/stubs-o32_hard.h', out/'usr/include/gnu/stubs-o32_hard_2008.h')
    lib, ulib = out/'lib', out/'usr/lib'
    shutil.copytree(rootfs/'lib', lib, symlinks=True)
    shutil.copytree(rootfs/'usr/lib', ulib, symlinks=True,
                    ignore=shutil.ignore_patterns('alsa-lib', 'engines-1.1', 'ts', 'terminfo'))

    for name in ['crt1.o', 'crti.o', 'crtn.o', 'Scrt1.o']:
        shutil.copy(gnu/'lib'/name, ulib/name)
        nan2008(ulib/name)
    work = out/'x-nonshared'
    work.mkdir()
    subprocess.run([f'llvm-ar{LLVM}', 'x', str(gnu/'lib/libc_nonshared.a')], cwd=work, check=True)
    members = sorted(work.iterdir())
    for m in members:
        nan2008(m)
    subprocess.run([f'llvm-ar{LLVM}', 'rcs', str(ulib/'libc_nonshared.a'), *[m.name for m in members]],
                   cwd=work, check=True)
    shutil.rmtree(work)

    (ulib/'libc.so').write_text('/* GNU ld script */\nOUTPUT_FORMAT(elf32-tradlittlemips)\n'
                                'GROUP ( /lib/libc.so.6 /usr/lib/libc_nonshared.a AS_NEEDED ( /lib/ld-linux-mipsn8.so.1 ) )\n')
    for so, real in [('libpthread.so', 'libpthread.so.0'), ('libm.so', 'libm.so.6'), ('libdl.so', 'libdl.so.2'),
                     ('librt.so', 'librt.so.1'), ('libgcc_s.so', 'libgcc_s.so.1'), ('libatomic.so', 'libatomic.so.1'),
                     ('libresolv.so', 'libresolv.so.2'), ('libcrypt.so', 'libcrypt.so.1'),
                     ('libstdc++.so', 'libstdc++.so.6')]:
        p = ulib/so
        if p.exists() or p.is_symlink():
            p.unlink()
        os.symlink('../../lib/' + real, p)

    # clang links -lgcc beside -lgcc_s, whose 64-bit helpers (__divdi3 ...) the Q2's libgcc_s.so.1 has.
    subprocess.run([f'llvm-ar{LLVM}', 'rcs', str(ulib/'libgcc.a')], check=True)
    empty = out/'empty.c'
    empty.write_text('')
    for name in ['crtbegin', 'crtbeginS', 'crtbeginT', 'crtend', 'crtendS']:
        subprocess.run([f'clang{LLVM}', '--target=mipsel-linux-gnu', '-march=mips32r2', '-mfp64', '-mnan=2008',
                        '-mabs=2008', '-c', str(empty), '-o', str(ulib/f'{name}.o')], check=True)
    empty.unlink()


if __name__ == '__main__':
    main()
