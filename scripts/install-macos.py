#!/usr/bin/env python3
"""Build and install a separate per-user launchd service; never writes credentials."""
import json
import os
import pathlib
import plistlib
import shutil
import subprocess
import time

project = pathlib.Path(__file__).resolve().parent.parent
state = pathlib.Path.home() / ".local" / "state" / "anthropic-body-proxy"
binary = pathlib.Path.home() / ".local" / "bin" / "anthropic-body-proxy"
label = "com.gwd.anthropic-body-proxy"
plist = pathlib.Path.home() / "Library" / "LaunchAgents" / (label + ".plist")

state.mkdir(parents=True, exist_ok=True, mode=0o700)
if state.is_symlink() or state.stat().st_mode & 0o077:
    raise SystemExit("Service state directory must be private (0700)")
for name in ["debug", "inspect"]:
    target = state / name
    target.mkdir(mode=0o700, exist_ok=True)
    if target.is_symlink() or target.stat().st_mode & 0o077:
        raise SystemExit("Debug and inspector directories must be private (0700)")
config = state / "body-policy.json"
if not config.exists():
    shutil.copyfile(project / "config.example.json", config)
    config.chmod(0o600)
binary.parent.mkdir(parents=True, exist_ok=True)
temporary_binary = binary.with_name(binary.name + ".new")
subprocess.run(["go", "build", "-trimpath", "-o", str(temporary_binary), "."], cwd=project, check=True)
temporary_binary.chmod(0o755)
temporary_binary.replace(binary)
definition = {
    "Label": label,
    "ProgramArguments": [str(binary), "-listen", "127.0.0.1:18083", "-config", str(config),
                         "-log", str(state / "requests.jsonl"), "-debug-dir", str(state / "debug"),
                         "-inspect-socket", str(state / "inspect" / "latest-request.sock")],
    "RunAtLoad": True,
    "KeepAlive": True,
    "ThrottleInterval": 5,
    "StandardOutPath": str(state / "service.log"),
    "StandardErrorPath": str(state / "service-error.log"),
}
plist.parent.mkdir(parents=True, exist_ok=True)
plist.write_bytes(plistlib.dumps(definition))
plist.chmod(0o600)
domain = "gui/" + str(os.getuid())
subprocess.run(["launchctl", "bootout", domain + "/" + label], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
# bootout can return before launchd finishes releasing the previous job.
for attempt in range(6):
    loaded = subprocess.run(["launchctl", "bootstrap", domain, str(plist)], capture_output=True, text=True)
    if loaded.returncode == 0:
        break
    if loaded.returncode != 5 or attempt == 5:
        raise SystemExit(loaded.stderr.strip() or "Could not bootstrap proxy service")
    time.sleep(0.25 * 2 ** attempt)
print(json.dumps({"service": label, "base_url": "http://127.0.0.1:18083", "state_directory": str(state)}))
