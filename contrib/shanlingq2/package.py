#!/usr/bin/env python3
"""Ready a staged card folder for the Q2 and zip it as a release ships it.

    package.py STAGED_DIR ZIP

STAGED_DIR is OUT/.airplay: the app q2-airplay, cliairplay, the launcher run, the manifest q2app and
the licence and source notes. The app must be a static MIPS32 little-endian executable with soft-float
ABI flags (GOMIPS=softfloat), so the firmware's -mfp64 doesn't matter; the Q2's kernel still requires
the nan2008 ELF flag, which Go doesn't set, so it is set here. cliairplay must be what
build-cliairplay.sh makes: hard float for the Q2 (FP64, NaN-2008), through its /lib/ld-linux-mipsn8.so.1.
Each entry is dated 2026-01-01, so two builds of a commit are byte-identical; System settings -> Apps
unpacks only the .airplay folder."""
import pathlib, struct, sys, zipfile

EF_MIPS_NAN2008 = 0x400
PT_INTERP, PT_MIPS_ABIFLAGS = 3, 0x70000003
MIPS_FP_SOFT, MIPS_FP_64 = 3, 6
STAMP = (2026, 1, 1, 0, 0, 0)
FILES = ('q2app', 'LICENSE', 'SOURCES', 'run', 'cliairplay', 'q2-airplay')  # the app last


def fail(msg):
    raise SystemExit(f'package.py: {msg}')


def elf(data, name):
    """data's interpreter (b'' when static), FP ABI and flags, checked as a little-endian MIPS32
    executable."""
    if data[:7] != b'\x7fELF\x01\x01\x01':
        fail(f'{name}: not a 32-bit little-endian ELF')
    e_type, e_machine = struct.unpack_from('<HH', data, 16)
    e_phoff, = struct.unpack_from('<I', data, 28)
    e_flags, = struct.unpack_from('<I', data, 36)
    e_phentsize, e_phnum = struct.unpack_from('<HH', data, 42)
    if e_type not in (2, 3) or e_machine != 8:  # an executable, or a position-independent one
        fail(f'{name}: not a MIPS executable')
    interp, fp = b'', None
    for i in range(e_phnum):
        p_type, p_offset, _, _, p_filesz = struct.unpack_from('<IIIII', data, e_phoff + i * e_phentsize)
        if p_type == PT_INTERP:
            interp = data[p_offset:p_offset + p_filesz].rstrip(b'\0')
        if p_type == PT_MIPS_ABIFLAGS:
            fp = data[p_offset + 7]
    return interp, fp, e_flags


def mark_app(data):
    interp, fp, flags = elf(data, 'q2-airplay')
    if interp:
        fail("q2-airplay: dynamically linked: the Q2 runs it without the build machine's libraries")
    if fp != MIPS_FP_SOFT:
        fail(f'q2-airplay: FP ABI {fp}, not soft float')
    struct.pack_into('<I', data, 36, flags | EF_MIPS_NAN2008)
    return data


def check_sender(data):
    interp, fp, flags = elf(data, 'cliairplay')
    if interp != b'/lib/ld-linux-mipsn8.so.1' or fp != MIPS_FP_64 or not flags & EF_MIPS_NAN2008:
        fail(f'cliairplay: not built for the Q2 ({interp!r}, FP ABI {fp}, flags {flags:#x})')
    return data


def main():
    if len(sys.argv) != 3:
        fail('usage: package.py STAGED_DIR ZIP')
    staged, out = pathlib.Path(sys.argv[1]), pathlib.Path(sys.argv[2])
    app = staged/'q2-airplay'
    app.write_bytes(mark_app(bytearray(app.read_bytes())))
    check_sender((staged/'cliairplay').read_bytes())
    with zipfile.ZipFile(out, 'w', zipfile.ZIP_DEFLATED) as z:
        for name in FILES:
            info = zipfile.ZipInfo(f'{staged.name}/{name}', STAMP)
            info.external_attr = (0o100000 | (0o755 if name in ('run', 'cliairplay', 'q2-airplay') else 0o644)) << 16
            info.compress_type = zipfile.ZIP_DEFLATED
            z.writestr(info, (staged/name).read_bytes())
    print(out)


if __name__ == '__main__':
    main()
