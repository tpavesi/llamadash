#!/usr/bin/env python3
"""Run the tests, build Linux binaries (CGO disabled) into dist/ and copy contrib/ and install.sh there."""
import os
import shutil
import struct
import subprocess
import sys
from pathlib import Path

ROOT = Path(__file__).resolve().parent
TARGETS = {"amd64": 0x3E, "arm64": 0xB7}  # GOARCH -> ELF e_machine
MAX_BYTES = 40 * 1000 * 1000  # a binary larger than this fails the build


def version() -> str:
    try:
        r = subprocess.run(["git", "describe", "--always", "--dirty"], cwd=ROOT,
                           capture_output=True, text=True, check=True)
        return r.stdout.strip() or "dev"
    except (OSError, subprocess.CalledProcessError):
        return "dev"


def check_elf(path: Path, machine: int) -> None:
    head = path.read_bytes()[:20]
    if len(head) < 20 or head[:4] != b"\x7fELF":
        sys.exit(f"{path}: not an ELF file")
    (got,) = struct.unpack_from("<H", head, 18)
    if got != machine:
        sys.exit(f"{path}: e_machine {got:#x}, want {machine:#x}")


def main() -> None:
    if subprocess.run(["go", "test", "./..."], cwd=ROOT).returncode != 0:
        sys.exit("tests failed")
    ver = version()
    out = ROOT / "dist"
    out.mkdir(exist_ok=True)
    for arch, machine in TARGETS.items():
        path = out / f"llamadash-linux-{arch}"
        env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=arch)
        cmd = ["go", "build", "-trimpath", "-ldflags", f"-s -w -X main.version={ver}",
               "-o", str(path), "./cmd/llamadash"]
        if subprocess.run(cmd, cwd=ROOT, env=env).returncode != 0:
            sys.exit(f"build failed for {arch}")
        check_elf(path, machine)
        size = path.stat().st_size
        print(f"{path}  {size / 1e6:.1f} MB  ELF ok (linux/{arch}), version {ver}")
        if size > MAX_BYTES:
            sys.exit(f"{path}: {size / 1e6:.1f} MB exceeds the {MAX_BYTES / 1e6:.0f} MB limit")
    shutil.copytree(ROOT / "contrib", out / "contrib", dirs_exist_ok=True)
    print(f"copied contrib/ to {out / 'contrib'}")
    # the installer goes next to the binaries, with Unix line endings
    script = (ROOT / "install.sh").read_bytes().replace(bytes([13, 10]), bytes([10]))
    (out / "install.sh").write_bytes(script)
    print(f"copied install.sh to {out / 'install.sh'}")


if __name__ == "__main__":
    main()
