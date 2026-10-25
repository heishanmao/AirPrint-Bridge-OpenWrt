#!/usr/bin/env python3
# Shared UCI/IPP helpers for the AirPrint bridge.
import os
import socket
import ssl
import struct
import time
from urllib.parse import urlsplit

UCI_PATH = "/etc/config/airprint"


def load_config(path=UCI_PATH, section="main"):
    """Minimal UCI reader: config airprint 'main' + option lines."""
    cfg = {}
    in_section = False
    try:
        with open(path, "r", encoding="utf-8") as f:
            for raw in f:
                line = raw.strip()
                if not line or line.startswith("#"):
                    continue
                if line.startswith("config "):
                    parts = line.split(None, 2)
                    in_section = (
                        len(parts) == 3
                        and parts[1] == "airprint"
                        and parts[2].strip("'\"") == section
                    )
                    continue
                if not in_section or not line.startswith("option "):
                    continue
                parts = line.split(None, 2)
                if len(parts) < 3:
                    continue
                value = parts[2].strip()
                if len(value) >= 2 and value[0] == value[-1] and value[0] in "'\"":
                    value = value[1:-1]
                cfg[parts[1]] = value
    except OSError:
        pass
    return cfg


def now():
    return time.time()


def iso(ts=None):
    return time.strftime("%Y-%m-%dT%H:%M:%S", time.localtime(ts or time.time()))


def human_duration(seconds):
    seconds = max(0, int(seconds))
    if seconds < 60:
        return "%ds" % seconds
    if seconds < 3600:
        return "%dm%02ds" % (seconds // 60, seconds % 60)
    return "%dh%02dm" % (seconds // 3600, (seconds % 3600) // 60)


def read_tail(path, lines=20, max_bytes=8192):
    try:
        with open(path, "rb") as f:
            f.seek(0, os.SEEK_END)
            size = f.tell()
            f.seek(max(0, size - max_bytes))
            data = f.read().decode("utf-8", "replace")
        return "\n".join(data.splitlines()[-lines:])
    except OSError:
        return ""


def tcp_check(host, port, timeout=1.2):
    started = time.time()
    try:
        s = socket.create_connection((host, int(port)), timeout)
        s.close()
        return True, round((time.time() - started) * 1000)
    except OSError:
        return False, None


def tls_check(host, port, timeout=3.0):
    """TCP plus TLS handshake: the printer accepts TCP well before its IPP stack is ready."""
    started = time.time()
    raw = None
    try:
        raw = socket.create_connection((host, int(port)), timeout)
        ctx = ssl.create_default_context()
        ctx.check_hostname = False
        ctx.verify_mode = ssl.CERT_NONE
        tls = ctx.wrap_socket(raw, server_hostname=host)
        tls.close()
        return True, round((time.time() - started) * 1000)
    except (OSError, ssl.SSLError, ValueError):
        try:
            if raw:
                raw.close()
        except OSError:
            pass
        return False, None


def ensure_dir(path, mode=0o777):
    try:
        os.makedirs(path, mode, exist_ok=True)
    except OSError:
        pass
    try:
        os.chmod(path, mode)
    except OSError:
        pass
    return path


def log_line(path, message, max_bytes=262144):
    try:
        ensure_dir(os.path.dirname(path))
        try:
            if os.path.getsize(path) > max_bytes:
                os.replace(path, path + ".1")
        except OSError:
            pass
        with open(path, "a", encoding="utf-8") as f:
            f.write("%s %s\n" % (iso(), message))
    except OSError:
        pass


def parse_uri(uri, default_scheme="ipps"):
    if not uri:
        uri = "%s://127.0.0.1:631/ipp/print" % default_scheme
    if "://" not in uri:
        uri = "%s://%s" % (default_scheme, uri)
    parts = urlsplit(uri)
    scheme = (parts.scheme or default_scheme).lower()
    host = parts.hostname or "127.0.0.1"
    try:
        port = parts.port or 631
    except ValueError:
        port = 631
    path = parts.path or "/ipp/print"
    return {
        "scheme": scheme,
        "host": host,
        "port": port,
        "path": path,
        "secure": scheme == "ipps",
        "uri": "%s://%s:%d%s" % (scheme, host, port, path),
    }


def ipp_status(data):
    if len(data) < 8:
        return None
    return struct.unpack(">H", data[2:4])[0]


# ---------- IPP encoding ----------

def attr(tag, name, value):
    n = name.encode("utf-8")
    v = value if isinstance(value, bytes) else str(value).encode("utf-8")
    return struct.pack(">BH", tag, len(n)) + n + struct.pack(">H", len(v)) + v


def attr_str(name, value, tag=0x44):
    return attr(tag, name, value)


def attr_int(name, value):
    return attr(0x21, name, struct.pack(">i", int(value)))


def attr_range(name, low, high):
    return attr(0x33, name, struct.pack(">ii", int(low), int(high)))


def attr_multi(name, values, tag=0x44):
    n = name.encode("utf-8")
    out = struct.pack(">B", tag) + struct.pack(">H", len(n)) + n
    for i, value in enumerate(values):
        v = value.encode("utf-8")
        if i:
            out += struct.pack(">B", tag) + struct.pack(">H", 0)
        out += struct.pack(">H", len(v)) + v
    return out


def parse_groups(data):
    """Parse an IPP message into [(group_tag, {name: [(value_tag, raw), ...]}), ...]."""
    groups = []
    if len(data) < 8:
        return groups
    pos = 8
    current = None
    cur = None
    while pos < len(data):
        tag = data[pos]
        pos += 1
        if tag == 0x03:
            break
        if tag < 0x10:
            current = {}
            cur = None
            groups.append((tag, current))
            continue
        if current is None:
            current = {}
            groups.append((0x01, current))
        if pos + 2 > len(data):
            break
        nlen = struct.unpack(">H", data[pos:pos + 2])[0]
        pos += 2
        if pos + nlen > len(data):
            break
        name = data[pos:pos + nlen].decode("utf-8", "replace") if nlen else ""
        pos += nlen
        if name:
            cur = name
        if pos + 2 > len(data):
            break
        vlen = struct.unpack(">H", data[pos:pos + 2])[0]
        pos += 2
        if pos + vlen > len(data):
            break
        value = data[pos:pos + vlen]
        pos += vlen
        if cur:
            current.setdefault(cur, []).append((tag, value))
    return groups


def parse_attrs(data):
    """Return {name: [(value_tag, raw_value), ...]} from an IPP message."""
    attrs = {}
    for _, group in parse_groups(data):
        for name, values in group.items():
            attrs.setdefault(name, []).extend(values)
    return attrs


def attr_text(attrs, name, default=""):
    values = attrs.get(name) or []
    if not values:
        return default
    return values[0][1].decode("utf-8", "replace")


def attr_number(attrs, name, default=0):
    values = attrs.get(name) or []
    if not values:
        return default
    tag, raw = values[0]
    if tag in (0x21, 0x23) and len(raw) == 4:
        return struct.unpack(">i", raw)[0]
    try:
        return int(raw.decode("ascii"))
    except (ValueError, UnicodeDecodeError):
        return default


def attr_bool(attrs, name, default=False):
    values = attrs.get(name) or []
    if not values:
        return default
    tag, raw = values[0]
    if tag == 0x22 and raw:
        return raw != b"\x00"
    text = raw.decode("ascii", "replace").strip().lower()
    if text in ("true", "1", "yes"):
        return True
    if text in ("false", "0", "no", ""):
        return False
    return default


def attr_list(attrs, name):
    return [raw.decode("utf-8", "replace") for _, raw in attrs.get(name, [])]


# ---------- IPP response rewriting ----------

def build_multi(name, values, tag=0x44):
    return attr_multi(name, values, tag)


def strip_attr(body, name):
    if len(body) < 8:
        return body
    key = name.encode("utf-8") if isinstance(name, str) else name
    out = bytearray(body[:8])
    pos = 8
    while pos < len(body):
        tag = body[pos]
        if tag == 0x03:
            out += body[pos:]
            break
        if tag < 0x10:
            out += body[pos:pos + 1]
            pos += 1
            continue
        if pos + 3 > len(body):
            out += body[pos:]
            break
        nlen = struct.unpack(">H", body[pos + 1:pos + 3])[0]
        if pos + 3 + nlen > len(body):
            out += body[pos:]
            break
        start = pos
        nm = body[pos + 3:pos + 3 + nlen]
        p = pos + 3 + nlen
        while p + 2 <= len(body):
            vlen = struct.unpack(">H", body[p:p + 2])[0]
            if p + 2 + vlen > len(body):
                break
            p += 2 + vlen
            if p >= len(body):
                break
            nt = body[p]
            if nt < 0x10 or nt in (0x01, 0x02, 0x03):
                break
            if nt == tag:
                n2 = struct.unpack(">H", body[p + 1:p + 3])[0]
                if n2 == 0:
                    p += 3
                    continue
            break
        if nm != key:
            out += body[start:p]
        pos = p
    return bytes(out)


def _append_attr(body, blob):
    idx = body.rfind(b"\x03")
    if idx < 0:
        idx = len(body)
    return body[:idx] + blob + body[idx:]


def _keyword_values(body, name):
    if len(body) < 8:
        return []
    pos = 8
    key = name.encode("utf-8")
    while pos < len(body):
        tag = body[pos]
        if tag == 0x03:
            break
        if tag < 0x10:
            pos += 1
            continue
        if pos + 3 > len(body):
            break
        nlen = struct.unpack(">H", body[pos + 1:pos + 3])[0]
        if pos + 3 + nlen > len(body):
            break
        nm = body[pos + 3:pos + 3 + nlen]
        p = pos + 3 + nlen
        values = []
        while p + 2 <= len(body):
            vlen = struct.unpack(">H", body[p:p + 2])[0]
            if p + 2 + vlen > len(body):
                break
            values.append(body[p + 2:p + 2 + vlen])
            p += 2 + vlen
            if p + 3 <= len(body):
                nt = body[p]
                n2 = struct.unpack(">H", body[p + 1:p + 3])[0]
                if nt == tag and n2 == 0:
                    p += 3
                    continue
            break
        if nm == key:
            return [v.decode("utf-8", "replace") for v in values]
        pos = p
    return []


def fix_job_reason(body):
    """CUPS 2.3.3 ends completed jobs with processing-to-stop-point."""
    if len(body) < 8:
        return body
    state = None
    reasons = []
    pos = 8
    while pos < len(body):
        tag = body[pos]
        if tag == 0x03:
            break
        if tag < 0x10:
            pos += 1
            continue
        nlen = struct.unpack(">H", body[pos + 1:pos + 3])[0]
        if pos + 5 + nlen > len(body):
            break
        name = body[pos + 3:pos + 3 + nlen].decode("utf-8", "replace")
        vlen = struct.unpack(">H", body[pos + 3 + nlen:pos + 5 + nlen])[0]
        value = body[pos + 5 + nlen:pos + 5 + nlen + vlen]
        pos += 5 + nlen + vlen
        if name == "job-state" and tag in (0x21, 0x23):
            state = int.from_bytes(value, "big", signed=True)
        elif name == "job-state-reasons":
            reasons.append(value.decode("utf-8", "replace"))
    if state is None:
        return body
    if state == 9:
        reason = "job-completed-with-errors" if any("error" in r for r in reasons) else "job-completed-successfully"
    elif state == 8:
        reason = "aborted-by-system"
    elif state == 7:
        reason = "job-canceled-by-user"
    elif state == 6:
        reason = "job-stopped"
    else:
        return body
    if reasons == [reason]:
        return body
    return _append_attr(strip_attr(body, b"job-state-reasons"), build_multi("job-state-reasons", [reason]))


def enhance_gpa(body, urf_values, airprint_values):
    for name in (b"urf-supported", b"ipp-features-supported", b"job-impressions-supported",
                 b"job-media-sheets-supported", b"printer-mandatory-job-attributes"):
        body = strip_attr(body, name)
    body = _append_attr(body, build_multi("urf-supported", urf_values))
    body = _append_attr(body, build_multi("ipp-features-supported", airprint_values))
    body = _append_attr(body, attr_range("job-impressions-supported", 1, 99999))
    body = _append_attr(body, attr_range("job-media-sheets-supported", 1, 99999))
    body = _append_attr(body, build_multi("printer-mandatory-job-attributes",
                                          ["job-impressions", "job-media-sheets"]))
    creation = _keyword_values(body, "job-creation-attributes-supported")
    if creation:
        for value in ("job-impressions", "job-media-sheets"):
            if value not in creation:
                creation.append(value)
        body = strip_attr(body, b"job-creation-attributes-supported")
        body = _append_attr(body, build_multi("job-creation-attributes-supported", creation))
    return body


def rewrite_uri_attrs(body, target):
    """Rewrite localhost/.local URIs to the endpoint the client used."""
    if len(body) < 8:
        return body

    def repl(value):
        for scheme in (b"ipp://", b"ipps://", b"http://", b"https://"):
            if value.startswith(scheme):
                rest = value[len(scheme):]
                slash = rest.find(b"/")
                authority = rest if slash < 0 else rest[:slash]
                host = authority.split(b":")[0].lower()
                if host in (b"127.0.0.1", b"localhost") or host.endswith(b".local"):
                    return scheme + target.encode("utf-8") + rest[len(authority):]
        return value

    out = bytearray(body[:8])
    pos = 8
    while pos < len(body):
        tag = body[pos]
        if tag == 0x03:
            out += body[pos:]
            break
        if tag < 0x10:
            out.append(tag)
            pos += 1
            continue
        if pos + 3 > len(body):
            out += body[pos:]
            break
        nlen = struct.unpack(">H", body[pos + 1:pos + 3])[0]
        if pos + 3 + nlen > len(body):
            out += body[pos:]
            break
        name = body[pos + 3:pos + 3 + nlen]
        p = pos + 3 + nlen
        values = []
        while True:
            if p + 2 > len(body):
                break
            vlen = struct.unpack(">H", body[p:p + 2])[0]
            if p + 2 + vlen > len(body):
                break
            values.append(body[p + 2:p + 2 + vlen])
            p += 2 + vlen
            if p + 3 <= len(body):
                nt = body[p]
                n2 = struct.unpack(">H", body[p + 1:p + 3])[0]
                if nt == tag and n2 == 0:
                    p += 3
                    continue
            break
        out += bytes([tag]) + struct.pack(">H", nlen) + name
        for i, value in enumerate(values):
            if tag == 0x45:
                value = repl(value)
            if i:
                out += bytes([tag]) + struct.pack(">H", 0)
            out += struct.pack(">H", len(value)) + value
        pos = p
    return bytes(out)


def http_ipp(host, port, path, body, timeout=8.0):
    """Small non-streaming IPP-over-HTTP request (monitor/status only)."""
    head = (
        "POST %s HTTP/1.1\r\nHost: %s:%d\r\nContent-Type: application/ipp\r\n"
        "Content-Length: %d\r\nConnection: close\r\nAccept-Encoding: identity\r\n\r\n"
        % (path, host, port, len(body))
    ).encode("ascii")
    s = socket.create_connection((host, int(port)), timeout)
    s.settimeout(timeout)
    s.sendall(head + body)
    data = b""
    while True:
        chunk = s.recv(65536)
        if not chunk:
            break
        data += chunk
    s.close()
    idx = data.find(b"\r\n\r\n")
    return data[idx + 4:] if idx >= 0 else b""
