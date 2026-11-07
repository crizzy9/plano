"""Synthetic, socket-free privacy and transport checks for the simulator proxy."""

import contextlib
from datetime import datetime, timezone
import importlib.util
import io
import json
from pathlib import Path
import runpy
import sys
import unittest
from unittest import mock


SPEC = importlib.util.spec_from_file_location("ios_demo_proxy", Path(__file__).with_name("ios-demo-proxy.py"))
proxy = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(proxy)
PRIVATE = "SYNTHETIC_PRIVATE_VALUE"


class FakeSocket:
    def __init__(self, data=b"", reader=None, write_error=None):
        self.reader = reader if reader is not None else io.BytesIO(data)
        self.output = bytearray()
        self.operations = []
        self.write_error = write_error

    def settimeout(self, value):
        self.operations.append(("timeout", value))

    def makefile(self, *args):
        return self.reader

    def sendall(self, data):
        if self.write_error is not None:
            raise self.write_error
        self.output.extend(data)


class FakeTLSSocket(FakeSocket):
    def __init__(self, *args, handshake_error=None, **kwargs):
        super().__init__(*args, **kwargs)
        self.handshake_error = handshake_error

    def do_handshake(self):
        self.operations.append(("handshake",))
        if self.handshake_error is not None:
            raise self.handshake_error


class FakeResponse:
    def __init__(self, status=200, body=b"{}", headers=None, error=None):
        self.status, self.body, self.error = status, body, error
        self.headers = headers if headers is not None else {"Content-Type": "application/json"}
        self.read_limits = []

    def read(self, limit):
        self.read_limits.append(limit)
        if self.error is not None:
            raise self.error
        return self.body[:limit]

    def getheader(self, name, default=None):
        return next((value for key, value in self.headers.items() if key.lower() == name.lower()), default)

    def getheaders(self):
        return list(self.headers.items())


class FakeUpstream:
    def __init__(self, response=None, header_error=None, request_error=None):
        self.response = response if response is not None else FakeResponse()
        self.header_error, self.request_error = header_error, request_error
        self.requests = []
        self.closed = False

    def request(self, method, target, *, body, headers):
        self.requests.append((method, target, body, headers))
        if self.request_error is not None:
            raise self.request_error

    def getresponse(self):
        if self.header_error is not None:
            raise self.header_error
        return self.response

    def close(self):
        self.closed = True


class TimeoutReader(io.BytesIO):
    def readline(self, size=-1):
        if self.tell() == len(self.getvalue()):
            raise TimeoutError(PRIVATE)
        return super().readline(size)


class BodyTimeoutReader(io.BytesIO):
    def read(self, size=-1):
        if size:
            raise TimeoutError(PRIVATE)
        return b""


def request(method="GET", target="/v1/tracked?credential=" + PRIVATE, headers="", body=b""):
    return (f"{method} {target} HTTP/1.1\r\nAuthorization: Bearer {PRIVATE}\r\n{headers}\r\n".encode() + body)


class ProxyTests(unittest.TestCase):
    def run_handler(self, socket, upstream=None):
        upstream = upstream if upstream is not None else FakeUpstream()
        output, errors = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors), \
                mock.patch.object(proxy.http.client, "HTTPConnection", return_value=upstream):
            proxy.DemoProxy(socket, (PRIVATE, 12345), object())
        self.assertEqual(errors.getvalue(), "")
        self.assertNotIn(PRIVATE, output.getvalue())
        events = [json.loads(line) for line in output.getvalue().splitlines()]
        for event in events:
            self.assertLessEqual(set(event), {"at_utc", "phase", "outcome", "route", "method", "elapsed_ms", "error_type", "status"})
            self.assertRegex(event["at_utc"], r"^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}\.[0-9]{6}\+00:00$")
            self.assertEqual(datetime.fromisoformat(event["at_utc"]).utcoffset(), timezone.utc.utcoffset(None))
            self.assertGreaterEqual(event["elapsed_ms"], 0)
        return events, upstream

    def event(self, events, phase, outcome):
        return next(event for event in events if event["phase"] == phase and event["outcome"] == outcome)

    def test_utc_event_timestamp_keeps_elapsed_time_monotonic(self):
        output = io.StringIO()
        with mock.patch.object(proxy, "datetime") as clock, \
                mock.patch.object(proxy.time, "monotonic", return_value=42.125), contextlib.redirect_stdout(output):
            clock.now.return_value = datetime(2026, 11, 7, 12, 34, 56, 123456, tzinfo=timezone.utc)
            proxy.diagnostic("request", "start", 42)
            clock.now.assert_called_once_with(timezone.utc)
        event = json.loads(output.getvalue())
        self.assertEqual(event["at_utc"], "2026-11-07T12:34:56.123456+00:00")
        self.assertEqual(event["elapsed_ms"], 125)

    def test_forwarding_is_unchanged_and_diagnostics_redact_all_values(self):
        body = ('{"credential":"' + PRIVATE + '"}').encode()
        response = FakeResponse(body=body, headers={
            "Content-Type": "application/json", "Content-Length": "999999", "Connection": "X-Private",
            "X-Private": PRIVATE, "Location": "/share/" + PRIVATE,
        })
        socket = FakeSocket(request())
        events, upstream = self.run_handler(socket, FakeUpstream(response))
        self.assertEqual(socket.operations[0], ("timeout", 10))
        self.assertEqual(upstream.requests[0], ("GET", "/v1/tracked?credential=" + PRIVATE, b"", {"Authorization": "Bearer " + PRIVATE}))
        self.assertTrue(upstream.closed)
        self.assertIn(b"HTTP/1.0 200", socket.output)
        self.assertIn(f"Content-Length: {len(body)}\r\n".encode(), socket.output)
        self.assertNotIn(b"X-Private:", socket.output)
        self.assertTrue(socket.output.endswith(body))
        self.assertEqual(response.read_limits, [proxy.MAX_RESPONSE_BYTES + 1])
        for phase in ("request", "upstream_request", "upstream_headers", "upstream_body", "downstream"):
            event = self.event(events, phase, "complete")
            self.assertEqual(event["route"], "tracked")
            self.assertEqual(event["method"], "GET")

    def test_unknown_route_and_method_are_generic(self):
        events, _ = self.run_handler(FakeSocket(request(target="/" + PRIVATE + "?token=" + PRIVATE)))
        self.assertEqual(self.event(events, "request", "start")["route"], "unknown")
        events, _ = self.run_handler(FakeSocket(request(method=PRIVATE)))
        self.assertTrue(all(event["method"] == "other" for event in events))
        self.assertEqual(self.event(events, "request_headers", "rejected")["status"], 501)

    def test_known_dynamic_routes_never_include_identifiers(self):
        for target, expected in (("/v1/flights/" + PRIVATE, "flight-detail"),
                                 ("/v1/flights/" + PRIVATE + "/radar", "flight-radar"),
                                 ("/v1/tracked/" + PRIVATE, "tracked-item"),
                                 ("/share/" + PRIVATE, "unknown")):
            with self.subTest(route=expected):
                events, _ = self.run_handler(FakeSocket(request(target=target)))
                self.assertEqual(self.event(events, "request", "start")["route"], expected)

    def test_idle_and_partial_headers_report_timeout_without_request_values(self):
        for data in (b"", f"GET /{PRIVATE} HTTP/1.1\r\nAuthorization: Bearer {PRIVATE}\r\n".encode()):
            with self.subTest(partial=bool(data)):
                events, upstream = self.run_handler(FakeSocket(reader=TimeoutReader(data)))
                self.assertEqual(self.event(events, "request_headers", "error")["error_type"], "TimeoutError")
                self.assertEqual(upstream.requests, [])
                self.assertFalse(any(event["phase"] == "request" for event in events))

    def test_partial_request_body_rejects_before_upstream(self):
        socket = FakeSocket(request("POST", headers="Content-Length: 5\r\n", body=b"ab"))
        events, upstream = self.run_handler(socket)
        self.event(events, "request_body", "partial")
        self.assertEqual(self.event(events, "request", "rejected")["status"], 400)
        self.assertEqual(upstream.requests, [])

    def test_request_body_timeout_preserves_408(self):
        socket = FakeSocket(reader=BodyTimeoutReader(request("POST", headers="Content-Length: 5\r\n")))
        events, upstream = self.run_handler(socket)
        self.assertEqual(self.event(events, "request_body", "error")["error_type"], "TimeoutError")
        self.assertEqual(self.event(events, "request", "rejected")["status"], 408)
        self.assertEqual(upstream.requests, [])

    def test_request_bounds_and_framing_stay_strict(self):
        for headers, status in (("Content-Length: 1\r\nContent-Length: 1\r\n", 400),
                                ("Transfer-Encoding: chunked\r\n", 400),
                                (f"Content-Length: {proxy.MAX_REQUEST_BYTES + 1}\r\n", 413)):
            with self.subTest(status=status):
                events, upstream = self.run_handler(FakeSocket(request("POST", headers=headers)))
                self.assertEqual(self.event(events, "request", "rejected")["status"], status)
                self.assertEqual(upstream.requests, [])

    def test_upstream_timeout_and_partial_body_are_distinguished_and_redacted(self):
        cases = ((FakeUpstream(request_error=TimeoutError(PRIVATE)), "upstream_request", "TimeoutError"),
                 (FakeUpstream(header_error=TimeoutError(PRIVATE)), "upstream_headers", "TimeoutError"),
                 (FakeUpstream(FakeResponse(error=proxy.http.client.IncompleteRead(PRIVATE.encode(), 100))), "upstream_body", "IncompleteRead"))
        for upstream, phase, error_type in cases:
            with self.subTest(phase=phase):
                events, _ = self.run_handler(FakeSocket(request()), upstream)
                self.assertEqual(self.event(events, phase, "error")["error_type"], error_type)
                self.assertEqual(self.event(events, "request", "rejected")["status"], 502)
                self.assertTrue(upstream.closed)

    def test_response_bound_and_head_no_body_statuses_are_preserved(self):
        with mock.patch.object(proxy, "MAX_RESPONSE_BYTES", 8):
            events, _ = self.run_handler(FakeSocket(request()), FakeUpstream(FakeResponse(body=b"x" * 9)))
        self.event(events, "upstream_body", "limit")
        self.assertEqual(self.event(events, "request", "rejected")["status"], 502)
        for method, status in (("HEAD", 200), ("GET", 204), ("GET", 304)):
            with self.subTest(method=method, status=status):
                socket = FakeSocket(request(method))
                self.run_handler(socket, FakeUpstream(FakeResponse(status, body=b"", headers={"Content-Length": "42"})))
                if method == "HEAD":
                    self.assertIn(b"Content-Length: 42\r\n", socket.output)
                else:
                    self.assertNotIn(b"Content-Length:", socket.output)
                self.assertTrue(socket.output.endswith(b"\r\n\r\n"))

    def test_downstream_disconnect_does_not_claim_completion(self):
        socket = FakeSocket(request(), write_error=BrokenPipeError(PRIVATE))
        events, _ = self.run_handler(socket)
        self.assertEqual(self.event(events, "downstream", "error")["error_type"], "BrokenPipeError")
        self.event(events, "request", "error")
        self.assertFalse(any(event["phase"] == "downstream" and event["outcome"] == "complete" for event in events))

    def test_tls_handshake_uses_existing_timeout_and_redacts_failure(self):
        with mock.patch.object(proxy.ssl, "SSLSocket", FakeTLSSocket):
            socket = FakeTLSSocket(request())
            events, _ = self.run_handler(socket)
            self.assertEqual(socket.operations[:2], [("timeout", 10), ("handshake",)])
            self.event(events, "handshake", "complete")
            socket = FakeTLSSocket(handshake_error=TimeoutError(PRIVATE))
            output = io.StringIO()
            with contextlib.redirect_stdout(output), self.assertRaises(TimeoutError):
                proxy.DemoProxy(socket, (PRIVATE, 12345), object())
            self.assertNotIn(PRIVATE, output.getvalue())
            events = [json.loads(line) for line in output.getvalue().splitlines()]
            self.assertEqual(self.event(events, "handshake", "error")["error_type"], "TimeoutError")

    def test_server_error_handler_never_prints_traceback_or_exception_message(self):
        output, errors = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors):
            try:
                raise ValueError(PRIVATE)
            except ValueError:
                proxy.DemoServer.handle_error(object(), object(), (PRIVATE, 12345))
        self.assertNotIn(PRIVATE, output.getvalue() + errors.getvalue())
        self.assertEqual(errors.getvalue(), "")
        self.assertEqual(json.loads(output.getvalue())["error_type"], "ValueError")

    def test_startup_tls_failure_never_logs_certificate_paths_or_exception_message(self):
        context = mock.Mock()
        context.load_cert_chain.side_effect = proxy.ssl.SSLError(PRIVATE)
        output, errors = io.StringIO(), io.StringIO()
        with mock.patch.object(sys, "argv", ["ios-demo-proxy.py", "--cert", PRIVATE, "--key", PRIVATE]), \
                mock.patch.object(proxy.ssl, "SSLContext", return_value=context), \
                contextlib.redirect_stdout(output), contextlib.redirect_stderr(errors), self.assertRaises(SystemExit) as result:
            runpy.run_path(str(Path(__file__).with_name("ios-demo-proxy.py")), run_name="__main__")
        self.assertEqual(result.exception.code, 1)
        self.assertNotIn(PRIVATE, output.getvalue() + errors.getvalue())
        self.assertEqual(errors.getvalue(), "")
        self.assertEqual(json.loads(output.getvalue())["error_type"], "SSLError")


if __name__ == "__main__":
    unittest.main()
