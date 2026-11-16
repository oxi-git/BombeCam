"""Check the files that make BombeCam installable as a Home Assistant add-on.

Home Assistant only finds out that a file is wrong when the store tries to
read it, and the add-on builder only fails after a long compile. These checks
run in a second, so a broken upload is caught before it reaches the store.

Usage: python3 addon/validate.py
"""

from __future__ import annotations

import re
import sys
from pathlib import Path

import yaml

ROOT = Path(__file__).resolve().parent.parent
ADDON = ROOT / "addon"

CONFIG_KEYS = {
    "name",
    "version",
    "slug",
    "description",
    "url",
    "arch",
    "startup",
    "boot",
    "host_network",
    "ingress",
    "init",
    "options",
    "schema",
}
REQUIRED_CONFIG_KEYS = {
    "name",
    "version",
    "slug",
    "description",
    "arch",
    "startup",
    "boot",
    "options",
    "schema",
}
KNOWN_ARCH = {"amd64", "aarch64", "armv7", "armhf", "i386"}
STARTUP_VALUES = {"initialize", "system", "services", "application", "once"}
BOOT_VALUES = {"auto", "manual"}
SLUG_RE = re.compile(r"^[a-z0-9][a-z0-9_-]*$")
VERSION_RE = re.compile(r"^\d+\.\d+\.\d+(-[0-9A-Za-z.-]+)?$")
SCHEMA_TYPES = {"str", "bool", "int", "float", "password", "email", "port", "url", "list"}
ADDON_BUILD_FILES = ("mediamtx.yml", "mediamtx.recording.yml", "run.sh")

problems: list[str] = []


def check(condition: bool, message: str) -> None:
    if not condition:
        problems.append(message)


def load_yaml(path: Path) -> object:
    try:
        with path.open(encoding="utf-8") as handle:
            return yaml.safe_load(handle)
    except FileNotFoundError:
        problems.append(f"{path.relative_to(ROOT)}: missing")
    except yaml.YAMLError as err:
        problems.append(f"{path.relative_to(ROOT)}: not valid YAML ({err})")
    return None


def check_config() -> dict:
    config = load_yaml(ADDON / "config.yaml")
    if not isinstance(config, dict):
        problems.append("addon/config.yaml: must be a mapping")
        return {}

    for key in sorted(REQUIRED_CONFIG_KEYS):
        check(key in config, f"addon/config.yaml: {key} is required")
    for key in sorted(set(config) - CONFIG_KEYS):
        problems.append(f"addon/config.yaml: unknown key {key}")

    slug = str(config.get("slug", ""))
    check(bool(SLUG_RE.match(slug)), f"addon/config.yaml: slug {slug!r} is not a valid slug")
    version = str(config.get("version", ""))
    check(
        bool(VERSION_RE.match(version)),
        f"addon/config.yaml: version {version!r} must look like 1.2.3",
    )
    check(
        bool(config.get("description")),
        "addon/config.yaml: description is empty",
    )
    check(
        len(str(config.get("description", ""))) <= 200,
        "addon/config.yaml: description should stay under 200 characters",
    )
    check(
        config.get("url", "").startswith("https://"),
        "addon/config.yaml: url must be an https:// link",
    )

    arch = config.get("arch") or []
    check(isinstance(arch, list) and bool(arch), "addon/config.yaml: arch lists the architectures to build")
    for name in arch if isinstance(arch, list) else []:
        check(name in KNOWN_ARCH, f"addon/config.yaml: unknown architecture {name!r}")
    check(
        config.get("startup") in STARTUP_VALUES,
        f"addon/config.yaml: startup must be one of {sorted(STARTUP_VALUES)}",
    )
    check(
        config.get("boot") in BOOT_VALUES,
        f"addon/config.yaml: boot must be one of {sorted(BOOT_VALUES)}",
    )
    # The gateway reaches MediaMTX on 127.0.0.1 and cameras push WebRTC over
    # ports we cannot list, so the add-on cannot work in its own network.
    check(
        config.get("host_network") is True,
        "addon/config.yaml: host_network must be true (MediaMTX and the gateway share 127.0.0.1)",
    )
    check(
        config.get("ingress", False) is False,
        "addon/config.yaml: ingress is not used; the gateway serves its own page on http_port",
    )
    check(
        config.get("init", False) is False,
        "addon/config.yaml: init must be false; run.sh supervises both processes",
    )

    options = config.get("options") or {}
    schema = config.get("schema") or {}
    check(isinstance(options, dict) and bool(options), "addon/config.yaml: options must be a mapping")
    check(isinstance(schema, dict) and bool(schema), "addon/config.yaml: schema must be a mapping")
    if not isinstance(options, dict) or not isinstance(schema, dict):
        return config

    for name in sorted(set(options) - set(schema)):
        problems.append(f"addon/config.yaml: option {name!r} has no schema")
    for name in sorted(set(schema) - set(options)):
        problems.append(f"addon/config.yaml: schema {name!r} has no default")
    for name, value in sorted(schema.items()):
        kind, _, rest = str(value).partition("(")
        if kind not in SCHEMA_TYPES:
            problems.append(f"addon/config.yaml: {name!r} has unknown schema type {kind!r}")
            continue
        if kind in {"int", "float"} and not rest.endswith(")"):
            problems.append(f"addon/config.yaml: {name!r} needs a range, like int(0,3650)")
        if kind == "list" and not rest.endswith(")"):
            problems.append(f"addon/config.yaml: {name!r} needs its values, like list(a|b)")
        default = options[name]
        if kind == "bool":
            check(isinstance(default, bool), f"addon/config.yaml: {name!r} defaults to {default!r}, expected true or false")
        elif kind in {"int", "float"}:
            check(isinstance(default, (int, float)) and not isinstance(default, bool), f"addon/config.yaml: {name!r} defaults to {default!r}, expected a number")
        elif kind == "port":
            check(isinstance(default, int) and 1 <= default <= 65535, f"addon/config.yaml: {name!r} defaults to {default!r}, expected a port")
        else:
            check(isinstance(default, str), f"addon/config.yaml: {name!r} defaults to {default!r}, expected text")
        if kind == "list":
            choices = rest.rstrip(")").split("|")
            check(default in choices, f"addon/config.yaml: {name!r} defaults to {default!r}, not one of {choices}")

    check(
        str(config.get("options", {}).get("admin_password", "")) == "",
        "addon/config.yaml: admin_password must start empty, not carry a default password",
    )
    check(
        str(config.get("options", {}).get("camera_ip", "")) == ""
        and str(config.get("options", {}).get("camera_subnet", "")) == "",
        "addon/config.yaml: camera_ip and camera_subnet must start empty",
    )
    return config


def check_translations(config: dict) -> None:
    schema = config.get("schema") or {}
    for language in ("en", "it"):
        path = ADDON / "translations" / f"{language}.yaml"
        data = load_yaml(path)
        if not isinstance(data, dict):
            problems.append(f"addon/translations/{language}.yaml: must be a mapping")
            continue
        configuration = data.get("configuration")
        if not isinstance(configuration, dict):
            problems.append(f"addon/translations/{language}.yaml: configuration section is missing")
            continue
        missing = sorted(set(schema) - set(configuration))
        extra = sorted(set(configuration) - set(schema))
        for name in missing:
            problems.append(f"addon/translations/{language}.yaml: {name!r} has no description")
        for name in extra:
            problems.append(f"addon/translations/{language}.yaml: {name!r} is not an option")
        for name in sorted(set(configuration) & set(schema)):
            entry = configuration[name] or {}
            if not isinstance(entry, dict) or not entry.get("name"):
                problems.append(f"addon/translations/{language}.yaml: {name!r} needs a name")


def check_run_script(config: dict) -> None:
    path = ADDON / "run.sh"
    if not path.is_file():
        problems.append("addon/run.sh: missing")
        return
    text = path.read_text(encoding="utf-8")
    check(text.startswith("#!/usr/bin/env bash"), "addon/run.sh: needs a bash shebang (the image installs bash)")
    check("set -euo pipefail" in text, "addon/run.sh: needs set -euo pipefail")
    for name in sorted(config.get("schema") or {}):
        check(name in text, f"addon/run.sh: option {name!r} is never read")
    # These keep the add-on's state across restarts; losing them loses the
    # administrator password, the saved Osaio keys and the stream settings.
    check("/data/options.json" in text, "addon/run.sh: must read /data/options.json")
    check("BOMBECAM_PROFILE_PATH" in text, "addon/run.sh: must keep the profile on /data")
    check("BOMBECAM_RECORDINGS_DIR" in text, "addon/run.sh: must tell the gateway where recordings live")


def check_dockerfile() -> None:
    path = ADDON / "Dockerfile"
    if not path.is_file():
        problems.append("addon/Dockerfile: missing")
        return
    text = path.read_text(encoding="utf-8")
    check("go run ./tools/setup -ldflags" in text, "addon/Dockerfile: must bake the Osaio keys with tools/setup -ldflags")
    for name in ADDON_BUILD_FILES:
        check(f"COPY {name} " in text, f"addon/Dockerfile: must copy {name} into the image")
    check("bluenviron/mediamtx:1.9.3" in text, "addon/Dockerfile: must take MediaMTX from the pinned bluenviron/mediamtx:1.9.3")
    check('ENTRYPOINT ["/usr/local/bin/run.sh"]' in text, "addon/Dockerfile: must start run.sh")
    check("bash " in text, "addon/Dockerfile: must install bash for run.sh")
    for name in ADDON_BUILD_FILES:
        check((ADDON / name).is_file(), f"addon/{name}: missing")


def check_repository() -> None:
    data = load_yaml(ROOT / "repository.yaml")
    if not isinstance(data, dict):
        problems.append("repository.yaml: must be a mapping")
        return
    for key in ("name", "url", "maintainer"):
        check(bool(str(data.get(key, "")).strip()), f"repository.yaml: {key} is required")
    check(
        str(data.get("url", "")).startswith("https://"),
        "repository.yaml: url must be the https:// address Home Assistant clones",
    )


def main() -> int:
    config = check_config()
    if config:
        check_translations(config)
        check_run_script(config)
    check_dockerfile()
    check_repository()

    if problems:
        for problem in problems:
            print(f"- {problem}")
        return 1
    print(f"add-on {config.get('slug')} {config.get('version')}: files are valid")
    return 0


if __name__ == "__main__":
    sys.exit(main())
