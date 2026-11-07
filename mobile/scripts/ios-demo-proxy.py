#!/usr/bin/env python3
"""Serve the local Plano demo over HTTPS for a disposable iOS simulator."""

import argparse
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import ssl


TIMEOUT_SECONDS = 10
MAX_REQUEST_BYTES = 1024 * 1024
MAX_RESPONSE_BYTES = 8 * 1024 * 1024
REQUEST_HEADERS = ("Accept", "Accept-Encoding", "Content-Type", "Authorization", "If-None-Match")
HOP_HEADERS = {
    "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
    "proxy-connection", "te", "trailer", "transfer-encoding", "upgrade",
}


class DemoProxy(BaseHTTPRequestHandler):
    timeout = TIMEOUT_SECONDS

    def log_message(self, format_string, *args):
        # Requests can contain installation credentials and private share URLs.
        pass

    def log_request(self, status="-", size="-"):
        print(f"{self.command} {status}", flush=True)

    def reject(self, status):
        self.send_response(status)
        self.send_header("Content-Length", "0")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True

    def forward(self):
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
        try:
            body = self.rfile.read(length)
        except TimeoutError:
            self.reject(408)
            return
        except OSError:
            self.close_connection = True
            return
        if len(body) != length:
            self.reject(400)
            return

        headers = {name: self.headers[name] for name in REQUEST_HEADERS if name in self.headers}
        upstream = http.client.HTTPConnection("127.0.0.1", 8080, timeout=TIMEOUT_SECONDS)
        try:
            upstream.request(self.command, self.path, body=body, headers=headers)
            response = upstream.getresponse()
            payload = response.read(MAX_RESPONSE_BYTES + 1)
        except (OSError, http.client.HTTPException):
            self.reject(502)
            return
        finally:
            upstream.close()
        if len(payload) > MAX_RESPONSE_BYTES:
            self.reject(502)
            return
        excluded = HOP_HEADERS | {"content-length"}
        excluded.update(name.strip().lower() for name in response.getheader("Connection", "").split(","))
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
        except OSError:
            self.close_connection = True

    do_GET = do_POST = do_PUT = do_PATCH = do_DELETE = do_HEAD = do_OPTIONS = forward


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cert", required=True, help="Local TLS certificate path")
    parser.add_argument("--key", required=True, help="Local TLS private key path")
    args = parser.parse_args()
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(args.cert, args.key)
    with ThreadingHTTPServer(("127.0.0.1", 8443), DemoProxy) as server:
        # Defer the handshake until the handler applies its socket timeout.
        server.socket = context.wrap_socket(server.socket, server_side=True, do_handshake_on_connect=False)
        try:
            server.serve_forever()
        except KeyboardInterrupt:
            pass


if __name__ == "__main__":
    main()
