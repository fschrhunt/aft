"""Build and verify four portable archives locally; never contact a release service."""

import argparse
import gzip
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[2]
TARGETS = (("linux", "amd64"), ("linux", "arm64"),
           ("darwin", "amd64"), ("darwin", "arm64"))


def release_notes(tag, changelog):
    """Require a stable version tag and one nonempty, exact changelog section."""
    if not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", tag):
        raise ValueError("expected a stable vX.Y.Z tag")
    matches = re.findall(r"^## " + re.escape(tag[1:]) + r"\n(.*?)(?=^## |\Z)",
                         changelog, re.M | re.S)
    if len(matches) != 1 or not matches[0].strip():
        raise ValueError(f"CHANGELOG.md needs exactly one nonempty '## {tag[1:]}' section")
    return matches[0].strip() + "\n"


def inspect_binary(binary, goos, goarch, revision):
    """Check a cross-built executable's target, static build and source revision."""
    info = json.loads(subprocess.check_output(
        ["go", "version", "-m", "-json", str(binary)], cwd=ROOT, text=True))
    settings = {entry["Key"]: entry["Value"] for entry in info["Settings"]}
    for key, expected in {"GOOS": goos, "GOARCH": goarch, "CGO_ENABLED": "0",
                          "-trimpath": "true", "vcs.revision": revision}.items():
        if settings.get(key) != expected:
            raise ValueError(f"{binary.name}: {key} must be {expected}")


def verify_uninstall(binary, home, env):
    """Exercise packaged setup/on/off/uninstall and preserve unrelated files byte for byte."""
    originals = {".profile": b"export EDITOR=vi\n", ".ssh/config": b"# keep SSH configuration\n",
                 "project/README.md": b"keep project files\n",
                 ".config/agent/settings.json": b"{}\n"}
    for name, content in originals.items():
        path = home / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(content)
    for args in (("setup", "--yes"), ("on", "--host", "local", "--dir", str(home / "project")),
                 ("off",), ("uninstall", "--yes")):
        subprocess.run([str(binary), *args], cwd=home / "project", env=dict(env, SHELL="/bin/sh"),
                       stdout=subprocess.PIPE, stderr=subprocess.STDOUT, check=True)
    remaining = {str(path.relative_to(home)): path.read_bytes()
                 for path in home.rglob("*") if path.is_file()}
    if remaining != originals or (home / ".aft").exists():
        raise ValueError("packaged uninstall did not restore the isolated home")


def verify_native(archive, version, built, server):
    """Run the extracted native client and server, then exercise real terminal sessions."""
    with tempfile.TemporaryDirectory(prefix="aft-artifact-") as temp:
        base = Path(temp)
        binary = base / "aft"
        with tarfile.open(archive) as bundle:
            binary.write_bytes(bundle.extractfile("aft").read())
        binary.chmod(0o700)
        home = base / "home"
        home.mkdir()
        env = dict(os.environ, HOME=str(home), AFT_HOME=str(home / ".aft"))
        # Keep Go caches while isolating the shell profiles and all Aft state.
        env.update(json.loads(subprocess.check_output(
            ["go", "env", "-json", "GOPATH", "GOMODCACHE", "GOCACHE"], cwd=ROOT, text=True)))
        actual = subprocess.check_output([str(binary), "version"], env=env, text=True).strip()
        if actual != f"aft {version}":
            raise ValueError(f"packaged client reports {actual!r}, expected aft {version}")
        info = json.loads(subprocess.check_output([str(server), "info"], env=env, text=True))
        if info["version"] != version or info["built"] != built:
            raise ValueError("embedded server version/build time does not match the release")
        # These tests run the archive's executable, not the test binary. Every
        # session uses a throwaway home and the reserved local host, never SSH.
        tests = "|".join(("TestHostShellsAreIndependent", "TestShellProjectVisibility",
                          "TestShellSessionSurvivesConnectionLoss", "TestCopyModeScrollback"))
        subprocess.run(["go", "test", "./cmd/aft", "-run", f"^({tests})$", "-count=1",
                        "-timeout", "2m"], cwd=ROOT,
                       env=dict(env, AFT_TEST_BINARY=str(binary)), check=True)
        uninstall_home = base / "uninstall-home"
        uninstall_home.mkdir()
        verify_uninstall(binary, uninstall_home,
                         dict(env, HOME=str(uninstall_home), AFT_HOME=str(uninstall_home / ".aft")))
    print("Native archive: metadata, terminal sessions and isolated uninstall passed.", flush=True)


def package(version, output, notes):
    """Build matching clients/servers and verify archive contents, execution and checksums."""
    output.mkdir(parents=True, exist_ok=False)
    revision = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    built = int(subprocess.check_output(["git", "log", "-1", "--format=%ct"], cwd=ROOT, text=True))
    host = tuple(subprocess.check_output(
        ["go", "env", "GOHOSTOS", "GOHOSTARCH"], cwd=ROOT, text=True).split())
    ldflags = (f"-s -w -X main.version={version} "
               f"-X github.com/fschrhunt/aft/internal/session.Version={version} "
               f"-X github.com/fschrhunt/aft/internal/session.Built={built}")
    subprocess.run(["./x", "servers", version], cwd=ROOT, check=True)
    checksums = []
    with tempfile.TemporaryDirectory(prefix="aft-build-") as temp:
        binary = Path(temp) / "aft"
        native_server = Path(temp) / "aft-server"
        for goos, goarch in TARGETS:
            blob = ROOT / f"internal/session/install/embed/aft-server-{goos}-{goarch}.gz"
            native_server.write_bytes(gzip.decompress(blob.read_bytes()))
            native_server.chmod(0o700)
            inspect_binary(native_server, goos, goarch, revision)
            env = dict(os.environ, GOOS=goos, GOARCH=goarch, CGO_ENABLED="0")
            subprocess.run(["go", "build", "-trimpath", "-ldflags", ldflags,
                            "-o", str(binary), "./cmd/aft"], cwd=ROOT, env=env, check=True)
            inspect_binary(binary, goos, goarch, revision)
            archive = output / f"aft-{version}-{goos}-{goarch}.tar.gz"
            with tarfile.open(archive, "w:gz") as bundle:
                for path in (binary, ROOT / "LICENSE", ROOT / "README.md", ROOT / "docs",
                             ROOT / "assets", ROOT / "CONTRIBUTING.md", ROOT / "AGENTS.md",
                             ROOT / "SECURITY.md", ROOT / "CHANGELOG.md"):
                    bundle.add(path, arcname=path.name)
            with tarfile.open(archive) as bundle:
                member = bundle.getmember("aft")
                if not member.isfile() or not member.mode & 0o111:
                    raise ValueError(f"{archive.name}: missing executable")
                if bundle.extractfile(member).read() != binary.read_bytes():
                    raise ValueError(f"{archive.name}: binary content mismatch")
                bundle.getmember("LICENSE")
                bundle.getmember("assets/lockup/white.svg")
            if (goos, goarch) == host:
                verify_native(archive, version, built, native_server)
            digest = hashlib.sha256(archive.read_bytes()).hexdigest()
            checksums.append(f"{digest}  {archive.name}\n")
            print(f"Built and inspected {archive.name}", flush=True)
    manifest = output / "checksums.txt"
    manifest.write_text("".join(checksums))
    for line in manifest.read_text().splitlines():
        expected, name = line.split("  ", 1)
        if hashlib.sha256((output / name).read_bytes()).hexdigest() != expected:
            raise ValueError(f"checksum mismatch: {name}")
    (output / "release-notes.md").write_text(notes + f"\nSource commit: {revision}\n")


def main():
    """Use temporary output for checks; refuse to overwrite any dist directory."""
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("check", "dist"))
    parser.add_argument("tag", nargs="?")
    args = parser.parse_args()
    if args.command == "dist" and not args.tag:
        parser.error("dist requires vX.Y.Z")
    if args.command == "dist" and subprocess.check_output(
            ["git", "status", "--porcelain"], cwd=ROOT, text=True).strip():
        raise ValueError("dist requires a clean committed checkout; use release-check for working changes")
    notes = release_notes(args.tag, (ROOT / "CHANGELOG.md").read_text()) if args.tag else "Unreleased verification build.\n"
    if args.command == "check":
        with tempfile.TemporaryDirectory(prefix="aft-release-check-") as temp:
            package(args.tag or "dev", Path(temp) / "archives", notes)
        print("Four-target release check passed; temporary archives removed.")
    else:
        output = ROOT / "dist" / args.tag
        package(args.tag, output, notes)
        print(f"Local release files: {output}")


if __name__ == "__main__":
    try:
        main()
    except (ValueError, FileExistsError) as error:
        raise SystemExit(str(error)) from error
