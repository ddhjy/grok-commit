"""Exercise real first-use TTY input without a Grok account or Git mutations."""
import http.server
import json
import os
import pathlib
import pty
import select
import shutil
import subprocess
import sys
import tempfile
import threading
import time


class Handler(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/models" or self.headers["Authorization"] != "Bearer fixture-hidden-key":
            self.send_error(401)
            return
        self.send_response(200)
        self.end_headers()
        self.wfile.write(b'{"data":[]}')

    def log_message(self, *_args):
        pass


server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
root = pathlib.Path(tempfile.mkdtemp(prefix="grok-commit-first-use-"))
master, slave = pty.openpty()
env = dict(os.environ, XAI_API_KEY="", GROK_COMMIT_AUTH="api",
           GROK_COMMIT_BASE_URL=f"http://127.0.0.1:{server.server_port}",
           GROK_COMMIT_CONFIG_DIR=str(root / "config"),
           GROK_COMMIT_STATE_DIR=str(root / "state"), GROK_COMMIT_AUTO_UPDATE="0")
process = subprocess.Popen([str(pathlib.Path(sys.argv[1]).resolve())], cwd=root,
                           env=env, stdin=slave, stdout=slave, stderr=slave)
os.close(slave)
output = b""
choice_sent = key_sent = False
deadline = time.monotonic() + 15
try:
    while time.monotonic() < deadline:
        ready, _, _ = select.select([master], [], [], 0.1)
        if ready:
            try:
                chunk = os.read(master, 4096)
            except OSError:
                break
            if not chunk:
                break
            output += chunk
        if b"Choose [1]:" in output and not choice_sent:
            os.write(master, b"1\n")
            choice_sent = True
        if b"xAI API key (hidden):" in output and not key_sent:
            # Send immediately: the terminal must already have echo disabled.
            os.write(master, b"fixture-hidden-key\n")
            key_sent = True
    process.wait(timeout=3)
    assert process.returncode == 0, output.decode()
    assert b"fixture-hidden-key" not in output, "API key was echoed"
    assert b"Ready!" in output, output.decode()
    saved = json.loads((root / "config" / "credentials.json").read_text())
    assert saved["api_key"] == "fixture-hidden-key"
    assert not (root / ".git").exists()
    print("First use: setup completed, immediate secret input stayed hidden, no Git changes.")
finally:
    if process.poll() is None:
        process.kill()
        process.wait()
    os.close(master)
    server.shutdown()
    shutil.rmtree(root)
