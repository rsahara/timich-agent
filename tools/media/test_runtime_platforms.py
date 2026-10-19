#!/usr/bin/env python3
"""Check public Make targets without running Docker or building media runtimes."""

import os
from pathlib import Path
import shlex
import subprocess
import unittest


AGENT_ROOT = Path(__file__).resolve().parents[2]
RUNTIMES = ("libvips", "ffmpeg")


def recipe_environment(runtime, action, host_os, host_arch, *overrides):
    # Simulate the host defaults even when CI itself runs on Linux.
    environment = os.environ.copy()
    for name in ("GOOS", "GOARCH", "DIST_OS", "DIST_ARCH", "MAKEFLAGS", "MFLAGS"):
        environment.pop(name, None)
    result = subprocess.run(
        ["make", "--no-print-directory", "-n", f"media-{runtime}-runtime-{action}",
         f"HOST_GOOS={host_os}", f"HOST_GOARCH={host_arch}",
         f"GOOS={host_os}", f"GOARCH={host_arch}", *overrides],
        cwd=AGENT_ROOT, env=environment, capture_output=True, text=True, check=True,
    )
    prefix = f"TIMICH_MEDIA_{runtime.upper()}_"
    return dict(
        token.split("=", 1) for token in shlex.split(result.stdout.replace("\\\n", ""))
        if token.startswith(prefix) and "=" in token
    )


class MediaRuntimePlatformTests(unittest.TestCase):
    def test_pack_targets_use_linux_for_mac_and_linux_hosts(self):
        for host_os in ("darwin", "linux"):
            for arch in ("amd64", "arm64"):
                for runtime in RUNTIMES:
                    with self.subTest(host_os=host_os, arch=arch, runtime=runtime):
                        values = recipe_environment(runtime, "pack", host_os, arch)
                        prefix = f"TIMICH_MEDIA_{runtime.upper()}_"
                        self.assertEqual(values[prefix + "DOCKER_PLATFORM"], f"linux/{arch}")
                        self.assertEqual(values[prefix + "PLATFORM"], f"linux_{arch}")
                        self.assertEqual(values[prefix + "RUNTIME_DIR"],
                                         f"media-runtime/{runtime}/linux_{arch}")

    def test_verify_targets_use_the_same_linux_platform(self):
        for host_os in ("darwin", "linux"):
            for arch in ("amd64", "arm64"):
                for runtime in RUNTIMES:
                    with self.subTest(host_os=host_os, arch=arch, runtime=runtime):
                        values = recipe_environment(runtime, "verify", host_os, arch)
                        self.assertEqual(values[f"TIMICH_MEDIA_{runtime.upper()}_DOCKER_PLATFORM"],
                                         f"linux/{arch}")

    def test_explicit_distribution_architecture_is_preserved(self):
        for host_os, host_arch, target_arch in (
            ("darwin", "arm64", "amd64"), ("linux", "amd64", "arm64"),
        ):
            for runtime in RUNTIMES:
                with self.subTest(host_os=host_os, target_arch=target_arch, runtime=runtime):
                    values = recipe_environment(runtime, "pack", host_os, host_arch,
                                                f"DIST_ARCH={target_arch}")
                    prefix = f"TIMICH_MEDIA_{runtime.upper()}_"
                    self.assertEqual(values[prefix + "DOCKER_PLATFORM"], f"linux/{target_arch}")
                    self.assertEqual(values[prefix + "PLATFORM"], f"linux_{target_arch}")


if __name__ == "__main__":
    unittest.main()
