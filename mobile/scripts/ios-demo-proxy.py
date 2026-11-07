#!/usr/bin/env python3
"""Serve the local Plano demo over HTTPS for a disposable iOS simulator."""

import argparse
from datetime import datetime, timezone
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import ssl
import sys
import threading
import time


TIMEOUT_SECONDS = 10
MAX_REQUEST_BYTES = 1024 * 1024
MAX_RESPONSE_BYTES = 8 * 1024 * 1024
REQUEST_HEADERS = ("Accept", "Accept-Encoding", "Content-Type", "Authorization", "If-None-Match")
HOP_HEADERS = {
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "proxy-connection", "te", "trailer", "transfer-encoding", "upgrade",
}
METHODS = {"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"}
ERROR_TYPES = {
    "TimeoutError", "OSError", "ConnectionResetError", "ConnectionAbortedError",
    "BrokenPipeError", "SSLError", "SSLEOFError", "SSLZeroReturnError",
    "SSLWantReadError", "SSLWantWriteError", "HTTPException", "IncompleteRead",
    "RemoteDisconnected", "BadStatusLine", "LineTooLong", "ValueError",
}
LOG_LOCK = threading.Lock()


def route_class(target):
    path = target.partition("?")[0]
    known = {
        "/healthz": "health", "/v1/installations": "installation",
        "/v1/flights": "flight-search", "/v1/tracked": "tracked",
        "/v1/devices": "device", "/v1/devices/capabilities": "device-capabilities",
    }
    if path in known:
        return known[path]
    parts = path.split("/")
    if len(parts) == 4 and parts[:3] == ["", "v1", "flights"] and parts[3]:
        return "flight-detail"
    if len(parts) == 5 and parts[:3] == ["", "v1", "flights"] and parts[3] and parts[4] == "radar":
        return "flight-radar"
    if len(parts) == 4 and parts[:3] == ["", "v1", "tracked"] and parts[3]:
        return "tracked-item"
    return "unknown"


def diagnostic(phase, outcome, started, *, route="unknown", method="other", error=None, status=None):
    # Never accept a raw URL, header, peer/request ID, body, or exception message.
    event = {"at_utc": datetime.now(timezone.utc).isoformat(timespec="microseconds"),
             "phase": phase, "outcome": outcome, "route": route, "method": method,
             "elapsed_ms": round(max(0, time.monotonic() - started) * 1000, 3)}
    if error is not None:
        name = type(error).__name__
        event["error_type"] = name if name in ERROR_TYPES else "Exception"
    if isinstance(status, int) and 100 <= status <= 599:
        event["status"] = status
    with LOG_LOCK:
        print(json.dumps(event, separators=(",", ":"), sort_keys=True), flush=True)


class DemoServer(ThreadingHTTPServer):
    def handle_error(self, request, client_address):
        # The default server handler prints a traceback and exception message.
        diagnostic("connection", "error", time.monotonic(), error=sys.exc_info()[1])


class DemoProxy(BaseHTTPRequestHandler):
    timeout = TIMEOUT_SECONDS

    def emit(self, phase, outcome, started, **fields):
        diagnostic(phase, outcome, started, route=getattr(self, "route", "unknown"),
                   method=self.command if getattr(self, "command", None) in METHODS else "other", **fields)

    def setup(self):
        self.connection_started = time.monotonic()
        self.route = "unknown"
        self.emit("connection", "start", self.connection_started)
        try:
            # StreamRequestHandler applies the existing timeout before TLS I/O.
            super().setup()
            if isinstance(self.connection, ssl.SSLSocket):
                started = time.monotonic()
                self.emit("handshake", "start", started)
                try:
                    self.connection.do_handshake()
                except OSError as error:
                    self.emit("handshake", "error", started, error=error)
                    raise
                self.emit("handshake", "complete", started)
        except OSError as error:
            self.emit("connection", "error", self.connection_started, error=error)
            raise

    def finish(self):
        try:
            super().finish()
        finally:
            self.emit("connection", "complete", self.connection_started)

    def handle_one_request(self):
        self.route = "unknown"
        self.request_started = time.monotonic()
        self.forwarded = self.header_error = False
        self.response_status = None
        self.emit("request_headers", "start", self.request_started)
        try:
            super().handle_one_request()
        except (OSError, http.client.HTTPException) as error:
            self.header_error = True
            self.emit("request_headers", "error", self.request_started, error=error)
            self.close_connection = True
        finally:
            if not self.forwarded and not self.header_error:
                outcome = "rejected" if self.response_status is not None else "eof"
                self.emit("request_headers", outcome, self.request_started, status=self.response_status)

    def log_message(self, format_string, *args):
        # Requests can contain installation credentials and private share URLs.
        pass

    def log_request(self, status="-", size="-"):
        self.response_status = status if isinstance(status, int) else None

    def log_error(self, format_string, *args):
        # BaseHTTPRequestHandler catches an idle/partial-header socket timeout.
        for error in args:
            if isinstance(error, TimeoutError):
                self.header_error = True
                self.emit("request_headers", "error", self.request_started, error=error)
                break

    def reject(self, status):
        started = time.monotonic()
        self.request_outcome = "rejected"
        self.emit("downstream", "start", started, status=status)
        try:
            self.send_response(status)
            self.send_header("Content-Length", "0")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.flush()
        except OSError as error:
            self.request_outcome = "error"
            self.emit("downstream", "error", started, status=status, error=error)
        else:
            self.emit("downstream", "complete", started, status=status)
        self.close_connection = True

    def forward(self):
        self.forwarded = True
        self.route = route_class(self.path)
        self.request_started = time.monotonic()
        self.request_outcome = "incomplete"
        self.emit("request", "start", self.request_started)
        try:
            self.forward_request()
        finally:
            self.emit("request", self.request_outcome, self.request_started, status=self.response_status)

    def forward_request(self):
        if not self.path.startswith("/") or self.headers.get("Transfer-Encoding"):
            self.reject(400)
            return
        lengths = self.headers.get_all("Content-Length", [])
        try:
            if len(lengths) > 1:
                raise ValueError
            length = int(lengths[0]) if lengths else 0
            if length < 0:
                raise ValueError
        except ValueError:
            self.reject(400)
            return
        if length > MAX_REQUEST_BYTES:
            self.reject(413)
            return
        started = time.monotonic()
        self.emit("request_body", "start", started)
        try:
            body = self.rfile.read(length)
        except TimeoutError as error:
            self.emit("request_body", "error", started, error=error)
            self.reject(408)
            return
        except OSError as error:
            self.emit("request_body", "error", started, error=error)
            self.request_outcome = "error"
            self.close_connection = True
            return
        if len(body) != length:
            self.emit("request_body", "partial", started)
            self.reject(400)
            return
        self.emit("request_body", "complete", started)

        headers = {name: self.headers[name] for name in REQUEST_HEADERS if name in self.headers}
        upstream = http.client.HTTPConnection("127.0.0.1", 8080, timeout=TIMEOUT_SECONDS)
        phase, started = "upstream_request", time.monotonic()
        self.emit(phase, "start", started)
        try:
            upstream.request(self.command, self.path, body=body, headers=headers)
            self.emit(phase, "complete", started)
            phase, started = "upstream_headers", time.monotonic()
            self.emit(phase, "start", started)
            response = upstream.getresponse()
            self.emit(phase, "complete", started, status=response.status)
            phase, started = "upstream_body", time.monotonic()
            self.emit(phase, "start", started)
            payload = response.read(MAX_RESPONSE_BYTES + 1)
        except (OSError, http.client.HTTPException) as error:
            self.emit(phase, "error", started, error=error)
            self.reject(502)
            return
        finally:
            upstream.close()
        if len(payload) > MAX_RESPONSE_BYTES:
            self.emit("upstream_body", "limit", started, status=response.status)
            self.reject(502)
            return
        self.emit("upstream_body", "complete", started, status=response.status)
        excluded = HOP_HEADERS | {"content-length"}
        excluded.update(name.strip().lower() for name in response.getheader("Connection", "").split(","))
        started = time.monotonic()
        self.emit("downstream", "start", started, status=response.status)
        try:
            self.send_response_only(response.status)
            self.log_request(response.status)
            for name, value in response.getheaders():
                if name.lower() not in excluded:
                    self.send_header(name, value)
            if response.status not in (204, 304):
                content_length = response.getheader("Content-Length", "0") if self.command == "HEAD" else str(len(payload))
                self.send_header("Content-Length", content_length)
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(payload)
            self.wfile.flush()
        except OSError as error:
            self.request_outcome = "error"
            self.emit("downstream", "error", started, status=response.status, error=error)
            self.close_connection = True
        else:
            self.request_outcome = "complete"
            self.emit("downstream", "complete", started, status=response.status)

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = do_OPTIONS = forward


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cert", required=True, help="Local TLS certificate path")
    parser.add_argument("--key", required=True, help="Local TLS private key path")
    args = parser.parse_args()
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(args.cert, args.key)
    with DemoServer(("127.0.0.1", 8443), DemoProxy) as server:
        # Defer the handshake until the handler applies its socket timeout.
        server.socket = context.wrap_socket(server.socket, server_side=True, do_handshake_on_connect=False)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        diagnostic("server", "error", time.monotonic(), error=error)
        raise SystemExit(1) from None
