#!/usr/bin/env python3
"""Check the add-on files the way Home Assistant reads them.

The add-on builder fails on a malformed config.yaml, but it does so only after
the user has installed the repository, and a wrong slug or version shows up as
a missing add-on. This script fails the build instead: unknown keys, options
without a schema, translations that drift from the schema, and a run.sh that
forgets an option.

Needs PyYAML, which every GitHub runner already has:

    python3 addon/validate.py
"""

import pathlib
import re
import sys

import yaml

ADDON = pathlib.Path(__file__).resolve().parent

# Keys Home Assistant's add-on schema knows. A typo here is not a warning: the
# builder rejects the whole add-on.
KNOWN_KEYS = {
    # required
    "name", "version", "slug", "description", "arch", "schema",
    # optional
    "url", "startup", "boot", "ports", "ports_description", "host_network",
    "host_ipc", "host_dbus", "host_pid", "webui", "watchdog", "ingress",
    "ingress_port", "ingress_entry", "ingress_stream", "panel_icon",
    "panel_title", "panel_admin", "map", "environment", "options", "backup",
    "backup_exclude", "backup_post", "backup_pre", "image", "timeout", "tmpfs",
    "privileged", "devices", "udev", "stage", "init", "advanced", "legacy",
    "apparmor", "kernel_modules", "services", "discovery", "hassio_role",
    "hassio_api", "homeassistant_api", "hostname", "auth_api", "docker_api",
    "stdin", "audio", "video", "gpio", "usb", "uart", "journald", "progress",
    "realtime", "available", "anonymize", "translations", "experimental",
    "location", "breaks", "supervisor_api",
}
ARCH_VALUES = {"amd64", "aarch64", "armv7", "armhf", "i386"}
STARTUP_VALUES = {"before", "after", "once", "services", "application"}
BOOT_VALUES = {"auto", "manual"}
SCHEMA_TYPES = {
    "str", "bool", "int", "float", "email", "url", "port", "device", "password",
    "list", "match", "select",
}

errors: list[str] = []


def fail(message: str) -> None:
    errors.append(message)


def load(path: pathlib.Path) -> object:
    try:
        return yaml.safe_load(path.read_text(encoding="utf-8")) or {}
    except yaml.YAMLError as error:
        fail(f"{path.name}: not valid YAML: {error}")
        return {}


def schema_type(entry: object) -> str:
    # list(cloud|mqtt), int(0,3650), str, password, ...
    match = re.match(r"([a-z]+)", str(entry))
    return match.group(1) if match else ""


def check_config() -> dict:
    config = load(ADDON / "config.yaml")
    if not isinstance(config, dict):
        fail("config.yaml: must be a map")
        return {}

    for key in ("name", "version", "slug", "description", "arch", "schema"):
        if key not in config:
            fail(f"config.yaml: missing required key {key!r}")
    for key in config:
        if key not in KNOWN_KEYS:
            fail(f"config.yaml: unknown key {key!r} (a typo here disables the add-on)")

    for key in ("name", "slug", "description"):
        if not str(config.get(key, "")).strip():
            fail(f"config.yaml: {key} is empty")

    slug = str(config.get("slug", ""))
    if slug and not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", slug):
        fail(f"config.yaml: slug {slug!r} must be lower case letters, digits, - and _")

    version = str(config.get("version", ""))
    if version and not re.fullmatch(r"\d+\.\d+\.\d+", version):
        fail(f"config.yaml: version {version!r} is not a plain x.y.z number")

    arch = config.get("arch")
    if not isinstance(arch, list) or not arch:
        fail("config.yaml: arch must list at least one architecture")
    else:
        for value in arch:
            if value not in ARCH_VALUES:
                fail(f"config.yaml: unknown architecture {value!r}")

    if config.get("startup", "application") not in STARTUP_VALUES:
        fail(f"config.yaml: startup {config.get('startup')!r} is not one of {sorted(STARTUP_VALUES)}")
    if config.get("boot", "auto") not in BOOT_VALUES:
        fail(f"config.yaml: boot {config.get('boot')!r} is not one of {sorted(BOOT_VALUES)}")

    # The gateway and MediaMTX talk over 127.0.0.1 and cameras push WebRTC to
    # random UDP ports: without the host namespace the add-on cannot work.
    if config.get("host_network") is not True:
        fail("config.yaml: host_network must be true (the gateway and MediaMTX share the host)")

    return config


def check_options(config: dict) -> dict:
    options = config.get("options") or {}
    schema = config.get("schema") or {}
    if not isinstance(options, dict):
        fail("config.yaml: options must be a map")
        options = {}
    if not isinstance(schema, dict):
        fail("config.yaml: schema must be a map")
        schema = {}

    for key in options:
        if key not in schema:
            fail(f"config.yaml: option {key!r} has no schema entry")
    for key in schema:
        if key not in options:
            fail(f"config.yaml: option {key!r} has a schema but no default")
    for key, entry in schema.items():
        kind = schema_type(entry)
        if kind not in SCHEMA_TYPES:
            fail(f"config.yaml: schema {key!r} uses unknown type {entry!r}")
            continue
        if kind == "int" and not re.fullmatch(r"int\(-?\d+,-?\d+\)", str(entry)):
            fail(f"config.yaml: int option {key!r} needs a range, got {entry!r}")
        if isinstance(entry, str) and kind == "list" and not re.fullmatch(r"list\((.*\|.*)?\)", entry):
            fail(f"config.yaml: list option {key!r} needs the allowed values, got {entry!r}")
    return schema


def check_translations(schema: dict) -> None:
    folder = ADDON / "translations"
    names = sorted(path.name for path in folder.glob("*.yaml"))
    if "en.yaml" not in names:
        fail("translations/en.yaml missing")
    for name in names:
        data = load(folder / name)
        configuration = data.get("configuration") if isinstance(data, dict) else None
        if not isinstance(configuration, dict):
            fail(f"translations/{name}: no configuration section")
            continue
        for key, entry in configuration.items():
            if not isinstance(entry, dict) or not str(entry.get("name", "")).strip():
                fail(f"translations/{name}: {key} has no name")
        missing = set(schema) - set(configuration)
        extra = set(configuration) - set(schema)
        if missing:
            fail(f"translations/{name}: no description for {sorted(missing)}")
        if extra:
            fail(f"translations/{name}: describes unknown options {sorted(extra)}")


def check_run_script(schema: dict) -> None:
    path = ADDON / "run.sh"
    if not path.is_file():
        fail("run.sh missing")
        return
    script = path.read_text(encoding="utf-8")
    for key in schema:
        if key not in script:
            fail(f"run.sh: option {key!r} is never read")
    if "#!/usr/bin/env bash" not in script:
        fail("run.sh: needs a bash shebang")
    if "set -euo pipefail" not in script:
        fail("run.sh: needs set -euo pipefail")


def check_repository() -> None:
    repository = load(ADDON / "repository.yaml")
    if not isinstance(repository, dict):
        fail("repository.yaml: must be a map")
        return
    for key in ("name", "url", "maintainer"):
        if not str(repository.get(key, "")).strip():
            fail(f"repository.yaml: {key} is empty")
    if repository.get("url") and not str(repository["url"]).startswith("https://"):
        fail("repository.yaml: url must be an https address")


def check_dockerfile() -> None:
    path = ADDON / "Dockerfile"
    if not path.is_file():
        fail("Dockerfile missing")
        return
    dockerfile = path.read_text(encoding="utf-8")
    if "tools/setup -ldflags" not in dockerfile:
        fail("Dockerfile: the Osaio server key is never read by the build")
    for name in ("mediamtx.yml", "mediamtx.recording.yml", "run.sh"):
        if not (ADDON / name).is_file():
            fail(f"addon/{name} missing")
        if f"COPY {name} " not in dockerfile:
            fail(f"Dockerfile: {name} is not copied into the image")
    if "ENTRYPOINT" not in dockerfile:
        fail("Dockerfile: no ENTRYPOINT")


def main() -> int:
    config = check_config()
    schema = check_options(config)
    check_translations(schema)
    check_run_script(schema)
    check_repository()
    check_dockerfile()

    for error in errors:
        print(f"error: {error}", file=sys.stderr)
    if errors:
        print(f"{len(errors)} problem(s) in the add-on files", file=sys.stderr)
        return 1
    print(f"add-on {config.get('slug')} {config.get('version')}: files are valid")
    return 0


if __name__ == "__main__":
    sys.exit(main())
