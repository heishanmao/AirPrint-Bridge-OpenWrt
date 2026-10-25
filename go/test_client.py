#!/usr/bin/env python3
"""Tiny IPP smoke test for the AirPrint proxy (works against 6310 or 6311)."""
import argparse
import http.client
import json
import os
import struct
import sys
import urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.join(HERE, "..", "package", "airprint-bridge", "root", "usr", "lib", "airprint"))
import common  # noqa: E402


def request(op, extra, reqid=1):
    return (struct.pack(">HHI", 0x0200, op, reqid) + b"\x01" +
            common.attr_str("attributes-charset", "utf-8", 0x47) +
            common.attr_str("attributes-natural-language", "en", 0x48) +
            extra + b"\x03")


def gpa_body(printer_uri):
    wanted = ("printer-name", "printer-state", "printer-state-reasons",
              "printer-uri-supported", "urf-supported", "ipp-features-supported",
              "job-creation-attributes-supported", "document-format-supported",
              "printer-mandatory-job-attributes")
    return request(0x000B,
                   common.attr_str("printer-uri", printer_uri, 0x45) +
                   common.attr_multi("requested-attributes", wanted))


def validate_body(printer_uri):
    return request(0x0004,
                   common.attr_str("printer-uri", printer_uri, 0x45) +
                   common.attr_str("requesting-user-name", "smoke-test", 0x42))


def create_body(printer_uri):
    return request(0x0005,
                   common.attr_str("printer-uri", printer_uri, 0x45) +
                   common.attr_str("requesting-user-name", "smoke-test", 0x42) +
                   common.attr_str("job-name", "go-smoke", 0x42))


def cancel_body(printer_uri, jid):
    return request(0x0008,
                   common.attr_str("printer-uri", printer_uri, 0x45) +
                   common.attr_int("job-id", jid) +
                   common.attr_str("requesting-user-name", "smoke-test", 0x42))


def post(host, port, path, body, chunked=False):
    conn = http.client.HTTPConnection(host, port, timeout=20)
    if chunked:
        conn.putrequest("POST", path)
        conn.putheader("Content-Type", "application/ipp")
        conn.putheader("Transfer-Encoding", "chunked")
        conn.endheaders()
        for i in range(0, len(body), 256):
            part = body[i:i + 256]
            conn.send(("%x\r\n" % len(part)).encode() + part + b"\r\n")
        conn.send(b"0\r\n\r\n")
    else:
        conn.request("POST", path, body=body, headers={"Content-Type": "application/ipp"})
    resp = conn.getresponse()
    data = resp.read()
    code = resp.status
    conn.close()
    return code, data


def show(label, code, data):
    status = common.ipp_status(data)
    attrs = common.parse_attrs(data)
    print("%-18s http=%s ipp=0x%04x %s" % (
        label, code, status if status is not None else -1,
        common.attr_text(attrs, "status-message", "")))
    return status, attrs


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--host", default="192.168.31.1")
    ap.add_argument("--port", type=int, default=6311)
    ap.add_argument("--path", default="/printers/EPSON_L3250")
    ap.add_argument("--printer-uri", default="ipp://127.0.0.1:631/printers/EPSON_L3250")
    ap.add_argument("--create-job", action="store_true", help="Create-Job then Cancel-Job (no paper)")
    ap.add_argument("--base-url", default="")
    args = ap.parse_args()
    base = args.base_url or ("http://%s:%d" % (args.host, args.port))

    print("== HTTP ==")
    with urllib.request.urlopen(base + "/healthz", timeout=5) as r:
        print("healthz", r.status, r.read().decode().strip(),
              "cors=", r.headers.get("Access-Control-Allow-Origin"))
    with urllib.request.urlopen(base + "/status.json", timeout=5) as r:
        status = json.loads(r.read())
        print("status", r.status, "level=", status.get("level"),
              "port=", status.get("config", {}).get("listen_port"),
              "cors=", r.headers.get("Access-Control-Allow-Origin"))
    try:
        urllib.request.urlopen(base + "/", timeout=5)
        print("root 200 (unexpected)")
    except urllib.error.HTTPError as exc:
        print("root", exc.code, "(expected 404)")
    conn = http.client.HTTPConnection(args.host, args.port, timeout=5)
    conn.request("OPTIONS", "/status.json")
    r = conn.getresponse()
    print("options", r.status, "cors=", r.headers.get("Access-Control-Allow-Origin"))
    conn.close()

    print("== GPA ==")
    code, data = post(args.host, args.port, args.path, gpa_body(args.printer_uri))
    st, attrs = show("gpa", code, data)
    for name in ("printer-name", "printer-state", "printer-uri-supported", "urf-supported",
                 "ipp-features-supported", "job-creation-attributes-supported",
                 "printer-mandatory-job-attributes"):
        values = common.attr_list(attrs, name)
        if len(values) > 6:
            values = values[:6] + ["..."]
        print("  %-38s %s" % (name, values))

    print("== chunked GPA ==")
    code, data = post(args.host, args.port, args.path, gpa_body(args.printer_uri), chunked=True)
    show("gpa-chunked", code, data)

    print("== Validate-Job ==")
    code, data = post(args.host, args.port, args.path, validate_body(args.printer_uri))
    show("validate", code, data)

    if args.create_job:
        print("== Create-Job ==")
        code, data = post(args.host, args.port, args.path, create_body(args.printer_uri))
        st, attrs = show("create", code, data)
        jid = common.attr_number(attrs, "job-id", 0)
        print("  job-id", jid)
        if jid:
            print("== Cancel-Job ==")
            code, data = post(args.host, args.port, args.path, cancel_body(args.printer_uri, jid))
            show("cancel", code, data)


if __name__ == "__main__":
    main()
