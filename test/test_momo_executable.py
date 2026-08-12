"""Momo test runner executable discovery tests."""

from pathlib import Path
from subprocess import CompletedProcess

import pytest

import momo
from momo import Momo, find_momo_executable, preferred_build_targets, select_momo_target


@pytest.mark.parametrize(
    "relative_path",
    [
        Path("release/momo/momo"),
        Path("release/momo/momo.exe"),
        Path("release/momo/Release/momo.exe"),
    ],
)
def test_find_momo_executable_supports_all_build_layouts(
    tmp_path: Path, relative_path: Path
) -> None:
    executable = tmp_path / relative_path
    executable.parent.mkdir(parents=True)
    executable.touch()

    assert find_momo_executable(tmp_path) == executable


def test_find_momo_executable_ignores_directories(tmp_path: Path) -> None:
    executable_directory = tmp_path / "release/momo/momo.exe"
    executable_directory.mkdir(parents=True)

    assert find_momo_executable(tmp_path) is None


@pytest.mark.parametrize(
    ("system", "machine", "expected"),
    [
        ("Windows", "AMD64", ["windows_x86_64"]),
        ("Windows", "ARM64", ["windows_x86_64"]),
        ("Linux", "x86_64", ["ubuntu-24.04_x86_64", "ubuntu-22.04_x86_64", "ubuntu-20.04_x86_64"]),
        ("Linux", "aarch64", ["ubuntu-24.04_armv8", "ubuntu-22.04_armv8", "ubuntu-20.04_armv8"]),
    ],
)
def test_preferred_build_targets(system: str, machine: str, expected: list[str]) -> None:
    assert preferred_build_targets(system, machine) == expected


def test_select_momo_target_prefers_windows_host_architecture() -> None:
    available = ["ubuntu-24.04_x86_64", "windows_x86_64"]

    assert select_momo_target(available, "Windows", "AMD64") == "windows_x86_64"
    assert select_momo_target(available, "Windows", "ARM64") == "windows_x86_64"


def test_select_momo_target_fallback_is_deterministic() -> None:
    assert select_momo_target(["z-target", "a-target"], "Unknown", "Unknown") == "a-target"


def test_select_momo_target_rejects_empty_list() -> None:
    with pytest.raises(ValueError, match="must not be empty"):
        select_momo_target([], "Windows", "AMD64")


def test_supports_cli_option_reads_stdout_and_stderr(monkeypatch: pytest.MonkeyPatch) -> None:
    Momo._supports_cli_option.cache_clear()
    calls = 0

    def fake_run(*_args, **_kwargs) -> CompletedProcess[str]:
        nonlocal calls
        calls += 1
        return CompletedProcess([], 0, stdout="base options", stderr="--fake-capture-device")

    monkeypatch.setattr(momo.subprocess, "run", fake_run)

    assert Momo._supports_cli_option("momo.exe", "--fake-capture-device")
    assert Momo._supports_cli_option("momo.exe", "--fake-capture-device")
    assert calls == 1
