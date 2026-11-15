#!/usr/bin/env python3
"""Mock OpenCode Go upstream used by scripts/smoke_test.sh.

Speaks just enough of the OpenAI-compatible API for the proxy: GET /v1/models and
POST /v1/chat/completions (JSON and SSE). Every request is appended to mock_log.jsonl
next to this file so the test can assert on the headers the proxy actually sent.

Usage: python3 test/mock_upstream.py [port]
"""
import json
import os
import sys
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

LOG = os.path.join(os.path.dirname(os.path.abspath(__file__)), "mock_log.jsonl")


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *args):
        pass

    def _record(self, body: bytes) -> None:
        with open(LOG, "a") as f:
            f.write(json.dumps({
                "method": self.command,
                "path": self.path,
                "headers": {k.lower(): v for k, v in self.headers.items()},
                "body": body.decode("utf-8", "replace"),
            }) + "\n")

    def do_GET(self):
        self._record(b"")
        payload = json.dumps({
            "object": "list",
            "data": [
                {"id": "deepseek-v4.1-flash", "object": "model", "created": 1, "owned_by": "opencode"},
                {"id": "kimi-k3", "object": "model", "created": 1, "owned_by": "opencode"},
            ],
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        body = self.rfile.read(length)
        self._record(body)
        try:
            req = json.loads(body or b"{}")
        except ValueError:
            req = {}

        if req.get("stream"):
            self._stream_sse()
            return

        payload = json.dumps({
            "id": "chatcmpl-mock",
            "object": "chat.completion",
            "created": 1,
            "model": req.get("model"),
            "choices": [{
                "index": 0,
                "message": {"role": "assistant", "content": "mock reply"},
                "finish_reason": "stop",
            }],
            "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            "echo_session": self.headers.get("x-opencode-session"),
            "echo_ua": self.headers.get("user-agent"),
            "echo_auth": self.headers.get("authorization"),
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def _stream_sse(self):
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Transfer-Encoding", "chunked")
        self.end_headers()
        for word in ["Hel", "lo", " from", " mock"]:
            chunk = ("data: " + json.dumps({
                "id": "chatcmpl-mock",
                "object": "chat.completion.chunk",
                "choices": [{"index": 0, "delta": {"content": word}, "finish_reason": None}],
            }) + "\n\n").encode()
            self.wfile.write(("%x\r\n" % len(chunk)).encode() + chunk + b"\r\n")
            self.wfile.flush()
            time.sleep(0.05)
        done = b"data: [DONE]\n\n"
        self.wfile.write(("%x\r\n" % len(done)).encode() + done + b"\r\n")
        self.wfile.write(b"0\r\n\r\n")
        self.wfile.flush()


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 9099
    ThreadingHTTPServer(("127.0.0.1", port), Handler).serve_forever()
