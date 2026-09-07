"""Exercise SDP completion racing with marker signaling shutdown on Windows."""

import base64
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import threading
import time
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

import pytest

from momo import find_momo_executable


@pytest.mark.skipif(sys.platform != "win32", reason="MLY2 requires Windows")
def test_marker_receiver_survives_close_while_creating_offer(tmp_path):
    executable = os.environ.get("TEST_MARKER_RECEIVER_EXECUTABLE")
    if not executable:
        executable = find_momo_executable(
            Path(__file__).resolve().parents[1] / "_build" / "windows_x86_64"
        )
    if not executable or not Path(executable).is_file():
        pytest.skip("Build a Windows Momo executable first")

    lock = threading.Lock()
    counts = {"manifests": 0, "connections": 0}

    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def do_GET(self):
            if self.path == "/manifest":
                with lock:
                    counts["manifests"] += 1
                    revision = counts["manifests"]
                body = json.dumps({
                    "version": 1,
                    "revision": str(revision),
                    "phase": "ready",
                    "sources": [
                        {"sourceId": f"test-{i}",
                         "observerPath": f"/ws/{i}?revision={revision}"}
                        for i in range(4)
                    ],
                }).encode()
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
                index = counts["connections"]
            # Exercise cancellation before and during asynchronous SDP creation.
            time.sleep((index % 4) * 0.001)
            payload = b'{"type":"close"}'
            try:
                self.wfile.write(bytes([0x81, len(payload)]) + payload)
                self.wfile.flush()
                time.sleep(0.02)
            except (BrokenPipeError, ConnectionResetError):
                pass
            self.close_connection = True

        def log_message(self, *_args):
            pass

    server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    log_path = tmp_path / "marker-shutdown.log"
    process = None
    try:
        with log_path.open("wb") as log:
            process = subprocess.Popen([
                str(executable), "--no-google-stun", "--log-level", "info",
                "p2p-marker-recv", "--manifest-url",
                f"http://127.0.0.1:{server.server_port}/manifest",
                "--manifest-poll-ms", "100", "--connect-parallelism", "4",
                "--mapping-name", f"Local\\MomoMarkerShutdownTest-{uuid.uuid4().hex}",
            ], stdout=log, stderr=subprocess.STDOUT,
                creationflags=subprocess.CREATE_NO_WINDOW)
            completed = False
            deadline = time.monotonic() + 90
            while time.monotonic() < deadline:
                if process.poll() is not None:
                    pytest.fail(f"Marker receiver exited: {process.returncode}\n"
                                + log_path.read_text(errors="replace")[-8000:])
                with lock:
                    completed = counts["connections"] >= 12
                if completed:
                    break
                time.sleep(0.05)
            assert completed, counts
            time.sleep(0.5)
            assert process.poll() is None, log_path.read_text(errors="replace")[-8000:]
    finally:
        if process is not None and process.poll() is None:
            process.terminate()
            process.wait(timeout=10)
        server.shutdown()
        server.server_close()
        thread.join(timeout=5)
