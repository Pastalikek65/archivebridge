#!/usr/bin/env python3
"""Run one CI command while retaining its raw streams and exact exit status."""

from __future__ import annotations

import argparse
from concurrent.futures import ThreadPoolExecutor
from datetime import datetime, timezone
import json
from pathlib import Path
import subprocess
import sys
import time
from typing import BinaryIO, Sequence


def _timestamp() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds").replace("+00:00", "Z")


def _pump(source: BinaryIO, target: Path, console: BinaryIO) -> None:
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("wb") as log:
        while True:
            block = source.read(64 * 1024)
            if not block:
                break
            log.write(block)
            log.flush()
            console.write(block)
            console.flush()


def _run(command: Sequence[str], stdout_path: Path, stderr_path: Path) -> tuple[int, str | None]:
    try:
        process = subprocess.Popen(command, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    except OSError as exc:
        stdout_path.parent.mkdir(parents=True, exist_ok=True)
        stderr_path.parent.mkdir(parents=True, exist_ok=True)
        stdout_path.write_bytes(b"")
        stderr_path.write_bytes((f"Could not start command: {exc}\n").encode("utf-8", "replace"))
        sys.stderr.buffer.write(stderr_path.read_bytes())
        return 127, f"{type(exc).__name__}: {exc}"

    assert process.stdout is not None
    assert process.stderr is not None
    with ThreadPoolExecutor(max_workers=2) as pool:
        stdout_task = pool.submit(_pump, process.stdout, stdout_path, sys.stdout.buffer)
        stderr_task = pool.submit(_pump, process.stderr, stderr_path, sys.stderr.buffer)
        while process.poll() is None:
            capture_failed = any(
                task.done() and task.exception() is not None
                for task in (stdout_task, stderr_task)
            )
            if capture_failed:
                process.kill()
                break
            time.sleep(0.05)
        return_code = process.wait()
        # Surface capture failures instead of reporting a successful command
        # when its evidence could not be retained.
        stdout_task.result()
        stderr_task.result()
    return return_code, None


def _parse_args(argv: Sequence[str] | None = None) -> argparse.Namespace:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--report", required=True, help="JSON command result report")
    parser.add_argument("--stdout-log", required=True, help="raw stdout capture path")
    parser.add_argument("--stderr-log", required=True, help="raw stderr capture path")
    parser.add_argument("command", nargs=argparse.REMAINDER, help="command to run after --")
    args = parser.parse_args(argv)
    if args.command and args.command[0] == "--":
        args.command = args.command[1:]
    if not args.command:
        parser.error("a command is required after --")
    resolved = [Path(args.report).resolve(), Path(args.stdout_log).resolve(), Path(args.stderr_log).resolve()]
    if len(set(resolved)) != len(resolved):
        parser.error("report and stream logs must use different paths")
    return args


def main(argv: Sequence[str] | None = None) -> int:
    args = _parse_args(argv)
    report_path = Path(args.report)
    stdout_path = Path(args.stdout_log)
    stderr_path = Path(args.stderr_log)
    report_path.parent.mkdir(parents=True, exist_ok=True)
    started_at = _timestamp()
    started = time.monotonic()
    try:
        return_code, spawn_error = _run(args.command, stdout_path, stderr_path)
    except OSError as exc:
        # A capture path can fail after the child starts. Stop it, preserve a
        # machine-readable failure, and never mask the capture failure.
        return_code = 125
        spawn_error = f"capture failed: {type(exc).__name__}: {exc}"

    payload = {
        "schemaVersion": 1,
        "command": args.command,
        "startedAt": started_at,
        "finishedAt": _timestamp(),
        "durationSeconds": round(time.monotonic() - started, 3),
        "exitCode": return_code,
        "stdoutLog": str(stdout_path),
        "stderrLog": str(stderr_path),
    }
    if spawn_error is not None:
        payload["error"] = spawn_error
    try:
        report_path.write_text(json.dumps(payload, indent=2) + "\n", encoding="utf-8")
    except OSError as exc:
        print(f"ci-test-report.py: cannot write command report: {exc}", file=sys.stderr)
        return 125
    return return_code


if __name__ == "__main__":
    raise SystemExit(main())
