"""Exercise real first-use TTY input without a Grok account or Git mutations."""
import http.server
import json
import os
import pathlib
import pty
import select
import shutil
import signal
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


def first_use(replies):
    """Run grok-commit on a new terminal, answering each prompt once in order.
    A reply of None presses Ctrl-C instead of typing."""
    master, slave = pty.openpty()
    env = dict(os.environ, XAI_API_KEY="", GROK_COMMIT_AUTH="api",
               GROK_COMMIT_BASE_URL=f"http://127.0.0.1:{server.server_port}",
               GROK_COMMIT_CONFIG_DIR=str(root / "config"),
               GROK_COMMIT_STATE_DIR=str(root / "state"), GROK_COMMIT_AUTO_UPDATE="0")
    process = subprocess.Popen([binary], cwd=root, env=env, stdin=slave, stdout=slave, stderr=slave)
    os.close(slave)
    output = b""
    replies = list(replies)
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
            if replies and replies[0][0] in output:
                reply = replies.pop(0)[1]
                if reply is None:
                    process.send_signal(signal.SIGINT)
                else:
                    os.write(master, reply)
        process.wait(timeout=3)
        return process.returncode, output.decode()
    finally:
        if process.poll() is None:
            process.kill()
            process.wait()
        os.close(master)


binary = str(pathlib.Path(sys.argv[1]).resolve())
server = http.server.HTTPServer(("127.0.0.1", 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
root = pathlib.Path(tempfile.mkdtemp(prefix="grok-commit-first-use-"))
try:
    code, output = first_use([(b"Choose 1 or 2 [1]:", b"1\n"),
                              # Send immediately: the terminal must already have echo disabled.
                              (b"Paste your xAI API key", b"fixture-hidden-key\n")])
    assert code == 0, output
    assert "fixture-hidden-key" not in output, "API key was echoed"
    for expected in ["nothing has been staged or committed yet", "Checking the connection to Grok... \u2713", "You're all set."]:
        assert expected in output, output
    saved = json.loads((root / "config" / "credentials.json").read_text())
    assert saved["api_key"] == "fixture-hidden-key"
    shutil.rmtree(root / "config")
    code, output = first_use([(b"Choose 1 or 2 [1]:", None)])
    assert code == 130, output
    assert "Setup cancelled. Nothing was saved" in output, output
    assert not (root / "config" / "credentials.json").exists()
    assert not (root / ".git").exists()
    print("First use: setup completed, immediate secret input stayed hidden, Ctrl-C exited cleanly, no Git changes.")
finally:
    server.shutdown()
    shutil.rmtree(root)
