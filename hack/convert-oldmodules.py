#!/usr/bin/env -S uv run --script
# /// script
# requires-python = ">=3.11"
# dependencies = ["pyyaml>=6.0", "ruamel.yaml>=0.18"]
# ///
"""
Convert OLDMODULES/<name>/metadata.yml files into Aileron VirtualMachineBuild
manifests under converted/<name>.yaml.

Mapping highlights:
  build.template          -> source.buildRef (base-windows10, base-linux, ...)
  build.memory            -> resources.memory
  build.manifest          -> network topology + per-VM nics
  build.steps (HCL)       -> provisioners[] (file/shell/powershell)
  build.servers[]         -> additional vms[] entries
  build.internet          -> network.vpcs[].internet
  build_files/<file>      -> spec.files[].inline (referenced by fileRef)

Lossy: rubric, briefing, helpdesk*, tags, teacherBriefing are dropped — they
have no home in the VirtualMachineBuild CRD.

Run with:
    hack/convert-oldmodules.py            # convert all
    hack/convert-oldmodules.py anaheim    # one module
"""

from __future__ import annotations

import argparse
import re
import sys
from collections import OrderedDict
from pathlib import Path

import io

import yaml
from ruamel.yaml import YAML
from ruamel.yaml.scalarstring import LiteralScalarString


REPO_ROOT = Path(__file__).resolve().parent.parent
OLDMOD_DIR = REPO_ROOT / "OLDMODULES"
DEFAULT_OUT = OLDMOD_DIR / "converted"

# Map old packer template names -> aileron base build names.
TEMPLATE_TO_BASE = {
    "windows10": "base-windows-10-mbqvn737-tnmm6",
    "windows11": "base-windows-11-ips809lg-s5v4m",
    "linux": "base-debian-momwhsei-mbfpl",
    "linux_graphical": "base-linux-graphical",
    "kali": "base-kali",
    "pfsense": "base-pfsense",
    "windows_server_2022": "base-windows-server-2022",
    "exam": "base-exam",
    "macosx": "base-macosx",
}

# Per-template communicator defaults (matches the packer template defaults).
TEMPLATE_COMMUNICATOR = {
    "windows10": {"shell": "powershell", "sshUsername": "skills", "sshPassword": "skills"},
    "windows11": {"shell": "powershell", "sshUsername": "skills", "sshPassword": "skills"},
    "windows_server_2022": {"shell": "powershell", "sshUsername": "skills", "sshPassword": "skills"},
    "linux": {"shell": "bash", "sshUsername": "skills", "sshPassword": "skills"},
    "linux_graphical": {"shell": "bash", "sshUsername": "skills", "sshPassword": "skills"},
    "kali": {"shell": "bash", "sshUsername": "skills", "sshPassword": "skills"},
    "pfsense": {"shell": "bash", "sshUsername": "admin", "sshPassword": "admin"},
    "exam": {"shell": "bash", "sshUsername": "skills", "sshPassword": "skills"},
    "macosx": {"shell": "bash", "sshUsername": "skills", "sshPassword": "skills"},
}

# Topologies: how the old `manifest` field maps to network + per-VM nics.
TOPOLOGIES = {
    "single-vm": {
        "internet": False,
        "subnets": [{"name": "main", "cidr": "10.0.1.0/24"}],
        "main_nics": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.10"}],
    },
    "single-vm-internet": {
        "internet": True,
        "subnets": [{"name": "main", "cidr": "10.0.1.0/24"}],
        "main_nics": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.10"}],
    },
    "single-vm-internet-bad-dns": {
        "internet": True,
        # bad-dns: route DNS through a non-resolver so the contestant has to fix it
        "subnets": [{"name": "main", "cidr": "10.0.1.0/24", "dns": "10.0.1.99"}],
        "main_nics": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.10"}],
    },
    "single-vm-e1000": {
        "internet": False,
        "subnets": [{"name": "main", "cidr": "10.0.1.0/24"}],
        "main_nics": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.10", "model": "e1000"}],
    },
    "single-vm-with-extra-disk": {
        "internet": False,
        "subnets": [{"name": "main", "cidr": "10.0.1.0/24"}],
        "main_nics": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.10"}],
        "main_extra_disk": True,
    },
    "vm-with-hidden-server": {
        "internet": False,
        "subnets": [{"name": "main", "cidr": "10.0.1.0/24"}],
        "main_nics": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.10"}],
        "servers": {
            "server": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.20"}],
        },
    },
    "vm-with-hidden-server-internet": {
        "internet": True,
        "subnets": [{"name": "main", "cidr": "10.0.1.0/24"}],
        "main_nics": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.10"}],
        "servers": {
            "server": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.20"}],
        },
    },
    "vm-with-extra-disk-and-hidden-server": {
        "internet": False,
        "subnets": [{"name": "main", "cidr": "10.0.1.0/24"}],
        "main_nics": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.10"}],
        "main_extra_disk": True,
        "servers": {
            "server": [{"name": "eth0", "subnet": "main", "ip": "10.0.1.20"}],
        },
    },
    "pfsense-lan": {
        "internet": False,
        "vpcs": [
            {"name": "uplink", "internet": False},
            {"name": "private", "internet": False},
        ],
        "subnets": [
            {"name": "uplink", "vpc": "uplink", "cidr": "10.0.1.0/24"},
            {"name": "lan", "vpc": "private", "cidr": "192.168.0.0/16", "dhcp": False},
        ],
        "main_nics": [{"name": "eth0", "subnet": "lan"}],
        "servers": {
            "router": [
                {"name": "eth0", "subnet": "uplink"},
                {"name": "eth1", "subnet": "lan"},
            ],
            "server": [{"name": "eth0", "subnet": "lan"}],
        },
    },
    "pfsense-lan-dmz": {
        "internet": False,
        "vpcs": [
            {"name": "uplink", "internet": False},
            {"name": "private", "internet": False},
        ],
        "subnets": [
            {"name": "uplink", "vpc": "uplink", "cidr": "10.0.1.0/24"},
            {"name": "lan", "vpc": "private", "cidr": "192.168.1.0/24", "dhcp": False},
            {"name": "dmz", "vpc": "private", "cidr": "10.1.1.0/24", "dhcp": False},
        ],
        "main_nics": [{"name": "eth0", "subnet": "lan"}],
        "servers": {
            "router": [
                {"name": "eth0", "subnet": "uplink"},
                {"name": "eth1", "subnet": "lan"},
                {"name": "eth2", "subnet": "dmz"},
            ],
            "dmznode": [{"name": "eth0", "subnet": "dmz"}],
            "server": [{"name": "eth0", "subnet": "lan"}],
        },
    },
}


# --- HCL provisioner parsing -------------------------------------------------


def parse_hcl_provisioners(hcl: str) -> list[dict]:
    """Walk through an HCL fragment and return a list of provisioner blocks
    each as {"type": str, "body": str}. Crude but works for the well-formed
    fragments in OLDMODULES — no string-with-brace edge cases observed there.
    """
    out = []
    i = 0
    while True:
        m = re.search(r'provisioner\s+"([^"]+)"\s*\{', hcl[i:])
        if not m:
            break
        ptype = m.group(1)
        start = i + m.end()
        depth = 1
        j = start
        while j < len(hcl) and depth > 0:
            c = hcl[j]
            if c == "{":
                depth += 1
            elif c == "}":
                depth -= 1
            j += 1
        body = hcl[start : j - 1]
        out.append({"type": ptype, "body": body})
        i = j
    return out


def extract_string(body: str, key: str) -> str | None:
    """Pull `key = "value"` out of a body."""
    m = re.search(rf'{key}\s*=\s*"((?:[^"\\]|\\.)*)"', body)
    if not m:
        return None
    return m.group(1).encode("utf-8").decode("unicode_escape")


def extract_string_list(body: str, key: str) -> list[str]:
    """Pull `key = ["a", "b"]` out of a body."""
    m = re.search(rf"{key}\s*=\s*\[(.*?)\]", body, re.DOTALL)
    if not m:
        return []
    items = re.findall(r'"((?:[^"\\]|\\.)*)"', m.group(1))
    return [s.encode("utf-8").decode("unicode_escape") for s in items]


PATH_VARS = re.compile(r"\$\{[^}]+\}/?")


def basename_from_packer_path(p: str) -> str:
    return PATH_VARS.sub("", p).lstrip("/")


# --- Conversion --------------------------------------------------------------


class Converter:
    def __init__(self, module_dir: Path):
        self.module_dir = module_dir
        self.name = module_dir.name
        self.metadata = yaml.safe_load((module_dir / "metadata.yml").read_text())
        self.files: "OrderedDict[str, dict]" = OrderedDict()

    def add_file(self, packer_path: str, *, scope: str | None = None) -> str:
        """Register a build_files/ asset; return its fileRef name.

        When `scope` is set (e.g. a server's location), look first under
        `<module>/<scope>/build_files/` and register the file with a
        `<scope>-<basename>` name to avoid colliding with a same-named file
        from the main VM. Fall back to `<module>/build_files/` (un-prefixed).
        """
        bn = basename_from_packer_path(packer_path)
        candidates: list[tuple[Path, str]] = []
        if scope:
            candidates.append(
                (self.module_dir / scope / "build_files" / bn, f"{scope}-{bn}")
            )
        candidates.append((self.module_dir / "build_files" / bn, bn))

        for src, name in candidates:
            if not src.exists():
                continue
            if name in self.files:
                return name
            try:
                content = src.read_text()
                self.files[name] = {"name": name, "inline": content}
            except UnicodeDecodeError:
                self.files[name] = {"name": name, "_binary": str(src)}
            return name

        # Not found anywhere — register as missing. Use the scoped name so a
        # missing scoped file doesn't accidentally alias a different missing
        # main-VM file.
        name = f"{scope}-{bn}" if scope else bn
        if name not in self.files:
            self.files[name] = {"name": name, "_missing": packer_path}
        return name

    def provisioners_from_steps(
        self, steps_hcl: str, shell: str, *, scope: str | None = None
    ) -> list[dict]:
        out: list[dict] = []
        inline_idx = 0
        for block in parse_hcl_provisioners(steps_hcl or ""):
            ptype = block["type"]
            body = block["body"]
            if ptype == "file":
                src = extract_string(body, "source")
                dst = extract_string(body, "destination")
                if src and dst:
                    ref = self.add_file(src, scope=scope)
                    out.append(
                        {
                            "type": "file",
                            "name": ref,
                            "file": {"fileRef": ref, "destination": dst},
                        }
                    )
            elif ptype in ("powershell", "shell"):
                scripts = extract_string_list(body, "scripts")
                inline_lines = extract_string_list(body, "inline")
                for s in scripts:
                    ref = self.add_file(s, scope=scope)
                    f = self.files.get(ref, {})
                    if "inline" in f:
                        out.append(
                            {
                                "type": "shell",
                                "name": Path(ref).stem,
                                "shell": {"inline": f["inline"]},
                            }
                        )
                    else:
                        # binary or missing — fall back to upload-and-run
                        tmp = (
                            f"C:\\Windows\\Temp\\{ref}"
                            if shell == "powershell"
                            else f"/tmp/{ref}"
                        )
                        out.append(
                            {
                                "type": "file",
                                "name": ref,
                                "file": {"fileRef": ref, "destination": tmp},
                            }
                        )
                        run_cmd = (
                            f"powershell -ExecutionPolicy Bypass -File {tmp}"
                            if shell == "powershell"
                            else f"bash {tmp}"
                        )
                        out.append(
                            {
                                "type": "shell",
                                "name": f"run-{Path(ref).stem}",
                                "shell": {"inline": run_cmd},
                            }
                        )
                if inline_lines:
                    inline_idx += 1
                    out.append(
                        {
                            "type": "shell",
                            "name": f"inline-{inline_idx}",
                            "shell": {"inline": "\n".join(inline_lines)},
                        }
                    )
            elif ptype == "breakpoint":
                continue
            else:
                out.append(
                    {
                        "type": "shell",
                        "name": f"unconverted-{ptype}",
                        "shell": {
                            "inline": f"# TODO: convert {ptype} provisioner\n# {body.strip()}"
                        },
                    }
                )
        return out

    def memory_to_quantity(self, mem) -> str:
        if not mem:
            return "4Gi"
        s = str(mem).strip()
        if re.match(r"^\d+(?:\.\d+)?(?:Mi|Gi|M|G)$", s):
            return s
        if s.isdigit():
            return f"{s}Mi"
        return s

    def build_vm(
        self,
        *,
        vm_name: str,
        template: str,
        memory,
        steps: str,
        nics: list,
        extra_disk: bool,
        scope: str | None = None,
    ) -> dict:
        comm = dict(TEMPLATE_COMMUNICATOR.get(template, {"shell": "bash"}))
        provisioners = self.provisioners_from_steps(
            steps, comm.get("shell", "bash"), scope=scope
        )
        vm: dict = {
            "name": vm_name,
            "source": {
                "buildRef": {
                    "name": TEMPLATE_TO_BASE.get(template, f"base-{template}")
                }
            },
            "resources": {"cpu": 2, "memory": self.memory_to_quantity(memory)},
            "communicator": comm,
        }
        # Windows base images were built with virtio-scsi (the OLDMODULES
        # kubevirt templates all set `bus: scsi`). Cloning the same disk on
        # virtio-blk causes Windows to BSOD with INACCESSIBLE_BOOT_DEVICE
        # because the boot-time storage driver doesn't match. Linux is happy
        # on virtio (its built-in default) so we leave it implicit there.
        rootdisk: dict = {"name": "rootdisk", "size": "25Gi"}
        if template.startswith("windows"):
            rootdisk["bus"] = "scsi"
        disks = [rootdisk]
        if extra_disk:
            disks.append({"name": "data", "size": "50Mi", "bus": "scsi"})
        vm["disks"] = disks
        if nics:
            vm["nics"] = nics
        if provisioners:
            vm["provisioners"] = provisioners
        return vm

    def convert(self) -> dict:
        build = self.metadata.get("build") or {}
        if not build:
            raise ValueError(f"{self.name}: no `build:` block")

        template = build.get("template")
        if not template:
            raise ValueError(f"{self.name}: no build.template")

        manifest_name = build.get("manifest") or "single-vm"
        topo = TOPOLOGIES.get(manifest_name)
        if topo is None:
            raise ValueError(f"{self.name}: unknown manifest {manifest_name!r}")

        internet = build.get("internet")
        if internet is None:
            internet = topo["internet"]

        network: dict = {"subnets": [dict(s) for s in topo["subnets"]]}
        vpcs = topo.get("vpcs")
        if vpcs:
            network["vpcs"] = [dict(v) for v in vpcs]
            for v in network["vpcs"]:
                if internet and v["name"] == "private":
                    v["internet"] = True
        else:
            network["vpcs"] = [{"name": "default", "internet": bool(internet)}]
            for s in network["subnets"]:
                s.setdefault("vpc", "default")

        main_vm_name = self._main_vm_name(template)
        main_vm = self.build_vm(
            vm_name=main_vm_name,
            template=template,
            memory=build.get("memory"),
            steps=build.get("steps", ""),
            nics=topo.get("main_nics", []),
            extra_disk=topo.get("main_extra_disk", False),
        )
        vms = [main_vm]

        servers_def = topo.get("servers", {})
        for srv in build.get("servers") or []:
            location = srv.get("location")
            stmpl = srv.get("template", "linux")
            srv_nics = servers_def.get(location, [])
            if not srv_nics:
                first_subnet = network["subnets"][0]["name"]
                srv_nics = [{"name": "eth0", "subnet": first_subnet}]
            vms.append(
                self.build_vm(
                    vm_name=location or stmpl,
                    template=stmpl,
                    memory=srv.get("memory"),
                    steps=srv.get("steps", ""),
                    nics=srv_nics,
                    extra_disk=False,
                    scope=location,
                )
            )

        # If the module has a prebuild.sh at its root, carry it along in the
        # files list. It isn't referenced by any provisioner — it's the
        # original out-of-band asset-generation script and is preserved for
        # reproducibility.
        prebuild = self.module_dir / "prebuild.sh"
        if prebuild.exists() and "prebuild.sh" not in self.files:
            try:
                self.files["prebuild.sh"] = {
                    "name": "prebuild.sh",
                    "inline": prebuild.read_text(),
                }
            except UnicodeDecodeError:
                self.files["prebuild.sh"] = {"name": "prebuild.sh", "_binary": str(prebuild)}

        spec: dict = {"network": network, "vms": vms}
        # Emit a spec.files entry for every referenced asset so fileRefs always
        # resolve. Files we couldn't inline (missing from disk, or binaries
        # that can't be represented as a YAML string) are pointed at the
        # external artifact dropbox so the build can still fetch them.
        files = []
        for f in self.files.values():
            entry = {k: v for k, v in f.items() if not k.startswith("_")}
            if "inline" not in entry and "url" not in entry:
                entry["url"] = (
                    f"https://dropbox.skillsusaits.org/artifacts/"
                    f"{self.name}/{entry['name']}"
                )
            files.append(entry)
        if files:
            spec["files"] = files
        spec["timeout"] = "60m"

        return {
            "apiVersion": "ruddervirt.io/v1alpha1",
            "kind": "VirtualMachineBuild",
            "metadata": {
                "name": self.name.replace("_", "-"),
            },
            "spec": spec,
        }

    def _main_vm_name(self, template: str) -> str:
        if template.startswith("windows"):
            return "workstation"
        if template == "pfsense":
            return "router"
        return "main"

    def warnings(self) -> list[str]:
        out = []
        for f in self.files.values():
            if "_missing" in f:
                out.append(f"missing build_file: {f['_missing']}")
            if "_binary" in f:
                out.append(
                    f"binary build_file inlined-as-empty: {f['_binary']} (host via URL instead)"
                )
        return out


# --- yaml output ------------------------------------------------------------


def _wrap_strings(obj):
    """Recursively wrap multi-line strings as LiteralScalarString so ruamel
    emits them as `|` block scalars. Trailing whitespace per line is stripped
    so block style is well-defined."""
    if isinstance(obj, dict):
        return {k: _wrap_strings(v) for k, v in obj.items()}
    if isinstance(obj, list):
        return [_wrap_strings(v) for v in obj]
    if isinstance(obj, str) and "\n" in obj:
        cleaned = "\n".join(line.rstrip() for line in obj.splitlines())
        if obj.endswith("\n"):
            cleaned += "\n"
        return LiteralScalarString(cleaned)
    return obj


def dump_yaml(manifest: dict) -> str:
    y = YAML()
    y.default_flow_style = False
    y.width = 100
    y.indent(mapping=2, sequence=2, offset=0)
    buf = io.StringIO()
    y.dump(_wrap_strings(manifest), buf)
    return buf.getvalue()


# --- main --------------------------------------------------------------------


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument(
        "--src",
        default=str(OLDMOD_DIR),
        help="OLDMODULES directory (default: %(default)s)",
    )
    ap.add_argument(
        "--out",
        default=str(DEFAULT_OUT),
        help="output directory (default: %(default)s)",
    )
    ap.add_argument(
        "modules",
        nargs="*",
        help="specific module names to convert (default: all that have metadata.yml)",
    )
    args = ap.parse_args()

    src = Path(args.src)
    out = Path(args.out)
    out.mkdir(parents=True, exist_ok=True)

    if args.modules:
        targets = [src / m for m in args.modules]
    else:
        targets = sorted(p.parent for p in src.glob("*/metadata.yml"))

    ok = 0
    failed = 0
    for module_dir in targets:
        if not (module_dir / "metadata.yml").exists():
            print(f"  skip: {module_dir.name} (no metadata.yml)", file=sys.stderr)
            continue
        try:
            c = Converter(module_dir)
            manifest = c.convert()
            dest = out / f"{module_dir.name}.yaml"
            dest.write_text(dump_yaml(manifest))
            warns = c.warnings()
            tag = "ok " if not warns else "warn"
            print(f"  {tag}: {module_dir.name} -> {dest.relative_to(REPO_ROOT)}")
            for w in warns:
                print(f"        {w}")
            ok += 1
        except Exception as e:
            failed += 1
            print(f"  fail: {module_dir.name}: {e}", file=sys.stderr)

    print(f"\nconverted {ok}, failed {failed}")
    return 0 if failed == 0 else 1


if __name__ == "__main__":
    sys.exit(main())
