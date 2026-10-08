"""Exercise installation in a temporary user directory; never loads a launchd job."""
import contextlib
import io
import json
import os
import pathlib
import plistlib
import runpy
import subprocess
import tempfile
import unittest
from unittest import mock


INSTALLER = pathlib.Path(__file__).with_name("install-macos.py")


class MacOSInstallationTests(unittest.TestCase):
    def install(self, directory):
        run = subprocess.run
        commands = []

        def execute(command, **kwargs):
            if command[0] == "launchctl":
                commands.append(command)
                return subprocess.CompletedProcess(command, 0, stdout="", stderr="")
            self.assertEqual(command[:2], ["go", "build"])
            return run(command, **kwargs)

        with mock.patch.object(pathlib.Path, "home", return_value=directory), mock.patch.object(
            subprocess, "run", side_effect=execute
        ), contextlib.redirect_stdout(io.StringIO()):
            runpy.run_path(str(INSTALLER), run_name="__main__")
        self.assertEqual([command[1] for command in commands], ["bootout", "bootstrap"])

    def test_fresh_install_matches_verified_service_configuration(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = pathlib.Path(temporary)
            self.install(directory)
            state = directory / ".local/state/anthropic-body-proxy"
            policy = state / "body-policy.json"
            self.assertEqual(json.loads(policy.read_text()), {
                "strip_claude_attribution": True,
                "strip_claude_code_identity": True,
                "drop_metadata_user_id": False,
                "max_output_tokens": 0,
            })
            self.assertEqual(policy.stat().st_mode & 0o777, 0o600)
            for name in ["debug", "inspect"]:
                self.assertEqual((state / name).stat().st_mode & 0o777, 0o700)
            plist = directory / "Library/LaunchAgents/com.gwd.anthropic-body-proxy.plist"
            definition = plistlib.loads(plist.read_bytes())
            self.assertTrue(definition["RunAtLoad"])
            self.assertTrue(definition["KeepAlive"])
            arguments = definition["ProgramArguments"]
            self.assertEqual(arguments[arguments.index("-listen") + 1], "127.0.0.1:18083")
            self.assertEqual(arguments[arguments.index("-debug-dir") + 1], str(state / "debug"))
            self.assertEqual(arguments[0], str(directory / ".local/bin/anthropic-body-proxy"))
            self.assertTrue(os.access(arguments[0], os.X_OK))
            self.assertFalse((directory / ".claude").exists())

    def test_reinstall_preserves_existing_policy_and_client_settings(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = pathlib.Path(temporary)
            state = directory / ".local/state/anthropic-body-proxy"
            state.mkdir(parents=True, mode=0o700)
            policy = state / "body-policy.json"
            policy_text = '{"strip_claude_attribution":false,"max_output_tokens":4096}\n'
            policy.write_text(policy_text)
            policy.chmod(0o600)
            settings = directory / ".claude/settings.json"
            settings.parent.mkdir(mode=0o700)
            settings_text = '{"env":{"ANTHROPIC_BASE_URL":"https://example.invalid"}}\n'
            settings.write_text(settings_text)
            self.install(directory)
            self.assertEqual(policy.read_text(), policy_text)
            self.assertEqual(settings.read_text(), settings_text)


if __name__ == "__main__":
    unittest.main()
