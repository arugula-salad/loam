#!/usr/bin/env python3
"""Install Fountain's locked Claude ACP tree, verifying every package before extraction."""
import base64
import csv
import hashlib
import io
from pathlib import Path
import platform
import subprocess
import tarfile
import urllib.request

VERSION = "0.81.2"
ROOT = Path("/home/sprite/.local/share/managoat/acp")
DEST = ROOT / f"claude-agent-acp@{VERSION}"
MANIFEST = Path(__file__).with_name(f"claude-agent-acp@{VERSION}.tsv")


def main():
    if platform.machine() not in ("x86_64", "amd64"):
        raise RuntimeError("this image's pinned ACP installation is linux-x64-glibc")
    DEST.mkdir(parents=True, exist_ok=False)
    for relative, url, integrity, system, cpu, libc in csv.reader(MANIFEST.open(), delimiter="\t"):
        if any(wanted != "-" and value not in wanted.split(",")
               for wanted, value in ((system, "linux"), (cpu, "x64"), (libc, "glibc"))):
            continue
        target = DEST / relative
        if not relative.startswith("node_modules/") or ".." in Path(relative).parts:
            raise ValueError("invalid manifest destination")
        if not url.startswith("https://registry.npmjs.org/"):
            raise ValueError("unexpected package registry")
        with urllib.request.urlopen(url, timeout=120) as response:
            data = response.read()
        algorithm, expected = integrity.split("-", 1)
        if algorithm != "sha512" or base64.b64encode(hashlib.sha512(data).digest()).decode() != expected:
            raise ValueError(f"package integrity mismatch: {relative}")
        target.mkdir(parents=True, exist_ok=True)
        with tarfile.open(fileobj=io.BytesIO(data), mode="r:gz") as archive:
            members = []
            for member in archive.getmembers():
                parts = Path(member.name).parts
                if not parts or parts[0] != "package" or ".." in parts:
                    raise ValueError("invalid package archive path")
                if len(parts) == 1:
                    continue
                member.name = str(Path(*parts[1:]))
                members.append(member)
            archive.extractall(target, members=members, filter="data")
    entry = DEST / "node_modules/@agentclientprotocol/claude-agent-acp/dist/index.js"
    entry.chmod(0o755)
    (DEST / ".node").write_text(subprocess.check_output(["node", "-p", "process.execPath"], text=True))
    launcher = DEST / "launch"
    launcher.write_text(f'''#!/bin/sh
d='{DEST}'
[ ! -f "$d/.claude-executable" ] || {{ read -r CLAUDE_CODE_EXECUTABLE <"$d/.claude-executable"; export CLAUDE_CODE_EXECUTABLE; }}
n=
[ -f "$d/.node" ] && read -r n <"$d/.node"
case $n in /*) [ -x "$n" ] || n=node ;; *) n=node ;; esac
exec "$n" '{entry}' "$@"
''')
    launcher.chmod(0o755)
    (DEST / ".installed").write_text(VERSION)
    link = Path("/home/sprite/.local/bin/claude-agent-acp")
    link.symlink_to(launcher)
    print(f"Installed verified Claude ACP {VERSION}")


if __name__ == "__main__":
    main()
