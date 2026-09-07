"""Keep marker video connections across race lifecycle metadata changes."""

import base64
import copy
import hashlib
import json
import mmap
import os
from pathlib import Path
import socket
import struct
import subprocess
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from momo import find_momo_executable


def revision_hash(value):
    result = 1469598103934665603
    for character in value.encode():
        result = ((result ^ character) * 1099511628211) & ((1 << 64) - 1)
    return result


@pytest.mark.skipif(sys.platform != "win32", reason="MLY2 requires Windows")
def test_manifest_preserves_streams_and_discards_superseded_topology(tmp_path):
    executable = os.environ.get("TEST_MARKER_RECEIVER_EXECUTABLE")
    if not executable:
        executable = find_momo_executable(
            Path(__file__).resolve().parents[1] / "_build" / "windows_x86_64"
        )
    if not executable or not Path(executable).is_file():
        pytest.skip("Build a Windows Momo executable first")

    sources = [
        {"sourceId": f"test-{i}", "carId": f"CP-{i}", "observerPath": f"/ws/{i}"}
        for i in range(2)
    ]
    manifest = {"version": 1, "revision": "initial", "phase": "ready", "sources": sources}
    lock = threading.Lock()
    stop = threading.Event()
    counts = {"manifests": 0, "connections": 0}
    served_revisions = {}

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def handle(self):
            try:
                super().handle()
            except ConnectionResetError:
                pass

        def do_GET(self):
            if self.path == "/manifest":
                with lock:
                    counts["manifests"] += 1
                    revision = manifest["revision"]
                    served_revisions[revision] = served_revisions.get(revision, 0) + 1
                    body = json.dumps(manifest).encode()
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)
                return
            key = self.headers.get("Sec-WebSocket-Key")
            if not self.path.startswith("/ws/") or not key:
                self.send_error(404)
                return
            accept = base64.b64encode(hashlib.sha1(
                (key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11").encode()
            ).digest()).decode()
            self.send_response(101)
            self.send_header("Upgrade", "websocket")
            self.send_header("Connection", "Upgrade")
            self.send_header("Sec-WebSocket-Accept", accept)
            self.end_headers()
            with lock:
                counts["connections"] += 1
            # No camera or production Relay: keep the signaling socket open.
            self.connection.settimeout(0.1)
            try:
                while not stop.is_set():
                    try:
                        if not self.connection.recv(65536):
                            break
                    except socket.timeout:
                        continue
            except (ConnectionResetError, OSError):
                pass
            self.close_connection = True

        def log_message(self, *_args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    mapping_name = f"Local\\MomoManifestTest-{uuid.uuid4().hex}"
    log_path = tmp_path / "marker-manifest.log"
    process = None
    mapping = None

    def wait_for(predicate):
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            assert process.poll() is None, log_path.read_text(errors="replace")[-8000:]
            if predicate():
                return
            time.sleep(0.01)
        pytest.fail("Manifest update did not settle\n"
                    + log_path.read_text(errors="replace")[-8000:])

    def snapshot():
        first_guard = struct.unpack_from("<q", mapping, 72)[0]
        if first_guard & 1:
            return None
        result = {
            "generation": struct.unpack_from("<q", mapping, 56)[0],
            "revision": struct.unpack_from("<Q", mapping, 80)[0],
            "phase": struct.unpack_from("<I", mapping, 48)[0],
            "count": struct.unpack_from("<I", mapping, 16)[0],
        }
        if struct.unpack_from("<q", mapping, 72)[0] != first_guard:
            return None
        return result

    def publish(revision, phase, next_sources, run="run-next", applied=True):
        with lock:
            previous_polls = served_revisions.get(revision, 0)
            manifest.update(revision=revision, phase=phase, raceRunId=run,
                            sources=copy.deepcopy(next_sources))
        # Multiple polls let the deferred branch execute before testing unlock.
        wait_for(lambda: served_revisions.get(revision, 0) >= previous_polls + 3)
        if applied:
            wait_for(lambda: (state := snapshot()) is not None
                     and state["revision"] == revision_hash(revision))

    try:
        with log_path.open("wb") as log:
            process = subprocess.Popen([
                str(executable), "--no-google-stun", "--log-level", "info",
                "p2p-marker-recv", "--manifest-url",
                f"http://127.0.0.1:{server.server_port}/manifest",
                "--manifest-poll-ms", "100", "--connect-parallelism", "4",
                "--connect-timeout-ms", "60000",
                "--mapping-name", mapping_name,
            ], stdout=log, stderr=subprocess.STDOUT,
                creationflags=subprocess.CREATE_NO_WINDOW)
            wait_for(lambda: counts["connections"] == 2)
            mapping = mmap.mmap(-1, 128, tagname=mapping_name, access=mmap.ACCESS_READ)
            wait_for(lambda: snapshot() is not None)
            generation = snapshot()["generation"]

            for index, phase in enumerate(("idle", "ready", "countdown", "green", "idle", "ready",
                                           "green", "finished", "idle")):
                publish(f"lifecycle-{index}", phase, sources, run=f"run-{index}")
                assert snapshot()["generation"] == generation
                assert counts["connections"] == 2

            reassigned = copy.deepcopy(sources)
            reassigned[0]["carId"] = "CP-9"
            publish("reassigned", "ready", reassigned)
            assert snapshot()["generation"] == generation
            assert counts["connections"] == 2

            extended = sources + [{"sourceId": "test-2", "observerPath": "/ws/2"}]
            publish("deferred", "green", extended, applied=False)
            assert snapshot()["phase"] == 3
            assert snapshot()["generation"] == generation
            assert snapshot()["count"] == 2
            # Abort returns the applied revision; an obsolete deferred roster must not win.
            publish("reassigned", "idle", reassigned)
            assert snapshot()["generation"] == generation
            assert snapshot()["count"] == 2
            assert counts["connections"] == 2

            publish("added", "ready", extended)
            generation += 1
            assert snapshot()["generation"] == generation
            assert snapshot()["count"] == 3
            publish("removed", "ready", sources)
            generation += 1
            assert snapshot()["generation"] == generation
            assert snapshot()["count"] == 2
            rerouted = copy.deepcopy(sources)
            rerouted[0]["observerPath"] = "/ws/replacement"
            publish("rerouted", "ready", rerouted)
            generation += 1
            assert snapshot()["generation"] == generation
            publish("empty", "idle", [])
            generation += 1
            assert snapshot()["generation"] == generation
            assert snapshot()["count"] == 0
            publish("empty-next", "ready", [])
            assert snapshot()["generation"] == generation
    finally:
        stop.set()
        if mapping is not None:
            mapping.close()
        if process is not None and process.poll() is None:
            process.terminate()
            process.wait(timeout=10)
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
