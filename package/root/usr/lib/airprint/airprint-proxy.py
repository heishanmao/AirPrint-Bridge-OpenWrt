#!/usr/bin/python3
"""AirPrint bridge: IPP capability proxy, job monitor, history and status page."""
import json
import logging
import os
import queue
import re
import shutil
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from logging.handlers import RotatingFileHandler
from urllib import request as urlrequest
from urllib.parse import quote, urlsplit

sys.path.insert(0, "/usr/lib/airprint")
import common

VERSION = "1.1.0"
RUNDIR = "/tmp/airprint"
URF_DIR = RUNDIR + "/urf"
BACKEND_LOG = RUNDIR + "/backend.log"
URF_LOG = RUNDIR + "/urf.log"
SIDECAR = RUNDIR + "/backend.json"
LOG_PATH = "/var/log/airprint/proxy.log"
CUPS_LOG = "/var/log/cups/error_log"

OP_PRINT_JOB = 0x0002
OP_VALIDATE = 0x0004
OP_CREATE = 0x0005
OP_GJA = 0x0009
OP_GET_JOBS = 0x000A
OP_GPA = 0x000B
SUBMIT_OPS = (OP_PRINT_JOB, OP_VALIDATE, OP_CREATE)

AIRPRINT_DEFAULT = (
    "subscription-object,airprint-1.8,airprint-1.7,airprint-1.6,"
    "airprint-1.5,airprint-1.4,airprint-1.3"
)

DEFAULTS = {
    "enabled": "1",
    "listen_port": "6310",
    "cups_host": "127.0.0.1",
    "cups_port": "631",
    "queue_name": "EPSON_L3250",
    "printer_uri": "ipps://192.168.31.6:631/ipp/print",
    "service_name": "EPSON L3250 Series",
    "service_uuid": "0ea66e10-844a-3a74-4c05-5b17c7daffa8",
    "dpi": "300",
    "note": "",
    "pdl": "image/urf,application/pdf,image/jpeg,image/png",
    "airprint_features": AIRPRINT_DEFAULT,
    "debug": "0",
    "auto_recover": "1",
    "offline_guard": "1",
    "notify_url": "",
    "notify_kind": "auto",
    "notify_events": "start,complete,error,recover",
    "history_file": "/var/log/airprint/jobs.jsonl",
    "history_max": "200",
}

STATE_TEXT = {
    3: "等待中",
    4: "已保留",
    5: "打印中",
    6: "已暂停",
    7: "已取消",
    8: "已中止",
    9: "已完成",
}

DIAG_PATTERNS = (
    (r"Filter failed", "过滤链失败：URF 转换或发送中断（打印机离线或忙）"),
    (r"rastertopwg (rc=|failed)", "PWG 栅格转换失败"),
    (r"raster data truncated|short PWG file", "打印数据不完整"),
    (r"Unable to connect|Connection refused|connection refused|timed out|timed out|handshake|TLS|SSL", "无法连接打印机 IPP"),
    (r"No space left", "存储空间不足"),
    (r"no pages in job|No pages", "任务没有可打印页面"),
    (r"job-canceled|Job canceled", "任务被取消"),
)


def _bool(value, default=True):
    if value is None:
        return default
    return str(value).strip().lower() not in ("0", "false", "no", "off", "")


def load_settings():
    raw = dict(DEFAULTS)
    raw.update(common.load_config())
    dpi = 600 if str(raw.get("dpi", "300")).strip() == "600" else 300
    features = [x.strip() for x in raw.get("airprint_features", "").split(",") if x.strip()]
    events = {x.strip() for x in raw.get("notify_events", "").split(",") if x.strip()}
    note = raw.get("note") or "AirPrint %ddpi via CUPS capability proxy" % dpi
    printer = common.parse_uri(raw.get("printer_uri", DEFAULTS["printer_uri"]))
    return {
        "listen_port": int(raw.get("listen_port") or 6310),
        "cups_host": raw.get("cups_host") or "127.0.0.1",
        "cups_port": int(raw.get("cups_port") or 631),
        "queue": raw.get("queue_name") or "EPSON_L3250",
        "printer": printer,
        "service_name": raw.get("service_name") or "EPSON L3250 Series",
        "uuid": raw.get("service_uuid") or DEFAULTS["service_uuid"],
        "dpi": dpi,
        "note": note,
        "pdl": raw.get("pdl") or DEFAULTS["pdl"],
        "urf_values": ["V1.4", "RS%d" % dpi, "DM1", "SRGB24", "W8"],
        "airprint_values": features or AIRPRINT_DEFAULT.split(","),
        "debug": _bool(raw.get("debug"), False),
        "auto_recover": _bool(raw.get("auto_recover"), True),
        "offline_guard": _bool(raw.get("offline_guard"), True),
        "notify_url": (raw.get("notify_url") or "").strip(),
        "notify_kind": (raw.get("notify_kind") or "auto").strip().lower(),
        "notify_events": events,
        "history_file": raw.get("history_file") or DEFAULTS["history_file"],
        "history_max": max(10, int(raw.get("history_max") or 200)),
    }


def setup_logging(debug):
    common.ensure_dir("/var/log/airprint", 0o755)
    logger = logging.getLogger("airprint")
    logger.setLevel(logging.DEBUG if debug else logging.INFO)
    logger.propagate = False
    try:
        handler = RotatingFileHandler(LOG_PATH, maxBytes=262144, backupCount=2, encoding="utf-8")
    except OSError:
        handler = logging.StreamHandler()
    handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(message)s", "%Y-%m-%d %H:%M:%S"))
    logger.addHandler(handler)
    return logger


def read_json(path):
    try:
        with open(path, "r", encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return {}


def write_json(path, data):
    tmp = path + ".tmp"
    try:
        with open(tmp, "w", encoding="utf-8") as f:
            json.dump(data, f, ensure_ascii=False, separators=(",", ":"))
        os.replace(tmp, path)
    except OSError:
        try:
            os.unlink(tmp)
        except OSError:
            pass


def age_text(seconds):
    seconds = max(0, int(seconds))
    if seconds < 60:
        return "%d 秒前" % seconds
    if seconds < 3600:
        return "%d 分钟前" % (seconds // 60)
    return "%d 小时前" % (seconds // 3600)


def newest_capture():
    try:
        names = [n for n in os.listdir(URF_DIR) if n.startswith("raw-") and n.endswith(".urf")]
        if not names:
            return None
        path = os.path.join(URF_DIR, max(names, key=lambda n: os.path.getmtime(os.path.join(URF_DIR, n))))
        st = os.stat(path)
        return {"path": path, "age": int(time.time() - st.st_mtime), "size": st.st_size}
    except OSError:
        return None


def proc_mem():
    total = avail = 0
    try:
        with open("/proc/meminfo", "r") as f:
            for line in f:
                if line.startswith("MemTotal:"):
                    total = int(line.split()[1])
                elif line.startswith("MemAvailable:"):
                    avail = int(line.split()[1])
    except (OSError, ValueError):
        pass
    return total, avail


def system_info(started):
    total, avail = proc_mem()
    try:
        load1 = float(open("/proc/loadavg").read().split()[0])
    except (OSError, ValueError):
        load1 = 0.0
    try:
        disk = shutil.disk_usage("/overlay")
        disk_free = disk.free // (1024 * 1024)
    except OSError:
        disk_free = 0
    uptime = time.time() - started
    return {
        "uptime_s": int(uptime),
        "uptime_text": common.human_duration(uptime),
        "mem_total_mb": total // 1024,
        "mem_avail_mb": avail // 1024,
        "load1": round(load1, 2),
        "disk_free_mb": disk_free,
    }


class Bridge:
    def __init__(self, settings, logger):
        self.s = settings
        self.log = logger
        self.lock = threading.RLock()
        self.started = time.time()
        self.last_client = None
        self.printer = {"ok": False, "ready": False, "ms": None, "checked": 0.0,
                        "ready_checked": 0.0}
        self.cups = {"ok": False, "state": None, "reasons": [], "accepting": None,
                     "queued": 0, "message": "", "checked": 0.0}
        self.active = {}
        self.history = []
        self.last_final = None
        self.last_recover = 0.0
        self.last_offline_notice = 0.0
        self.notify_q = queue.Queue()
        self.stop_event = threading.Event()
        common.ensure_dir(RUNDIR)
        common.ensure_dir(URF_DIR)
        self._load_history()
        self._thread(self._monitor_loop)
        self._thread(self._notify_loop)

    @staticmethod
    def _thread(target):
        threading.Thread(target=target, daemon=True).start()

    # ---------- history ----------

    def _load_history(self):
        path = self.s["history_file"]
        try:
            common.ensure_dir(os.path.dirname(path), 0o755)
            with open(path, "r", encoding="utf-8") as f:
                lines = f.readlines()
        except OSError:
            return
        records = []
        for line in lines[-self.s["history_max"]:]:
            try:
                records.append(json.loads(line))
            except ValueError:
                continue
        with self.lock:
            self.history = records
            self.last_final = records[-1] if records else None
        if len(lines) > self.s["history_max"]:
            self._rewrite_history()

    def _rewrite_history(self):
        path = self.s["history_file"]
        tmp = path + ".tmp"
        try:
            with open(tmp, "w", encoding="utf-8") as f:
                for record in self.history[-self.s["history_max"]:]:
                    f.write(json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n")
            os.replace(tmp, path)
        except OSError as exc:
            self.log.warning("history rewrite failed: %s", exc)

    def _append_history(self, record):
        with self.lock:
            self.history.append(record)
            over = len(self.history) > self.s["history_max"]
            if over:
                self.history = self.history[-self.s["history_max"]:]
            self.last_final = record
        path = self.s["history_file"]
        try:
            common.ensure_dir(os.path.dirname(path), 0o755)
            with open(path, "a", encoding="utf-8") as f:
                f.write(json.dumps(record, ensure_ascii=False, separators=(",", ":")) + "\n")
        except OSError as exc:
            self.log.warning("history append failed: %s", exc)
        if over:
            self._rewrite_history()

    # ---------- notifications ----------

    def notify(self, event, title, body):
        if event not in self.s["notify_events"] or not self.s["notify_url"]:
            return
        self.notify_q.put((title, body))

    def _notify_loop(self):
        while not self.stop_event.is_set():
            try:
                title, body = self.notify_q.get(timeout=2)
            except queue.Empty:
                continue
            try:
                self._send_notify(title, body)
            except Exception as exc:
                self.log.warning("notify failed: %r", exc)

    def _send_notify(self, title, body):
        url = self.s["notify_url"]
        kind = self.s["notify_kind"]
        title = title[:64]
        body = body[:300]
        if kind == "auto":
            kind = "bark" if "api.day.app" in url else "ntfy"
        if kind == "bark":
            target = url.rstrip("/") + "/" + quote(title, safe="") + "/" + quote(body, safe="") + "?group=airprint"
            req = urlrequest.Request(target, method="GET")
            with urlrequest.urlopen(req, timeout=6) as resp:
                resp.read(2048)
            return
        if kind == "generic":
            payload = json.dumps({"title": title, "body": body}, ensure_ascii=False).encode("utf-8")
            req = urlrequest.Request(url, data=payload, method="POST",
                                     headers={"Content-Type": "application/json"})
            with urlrequest.urlopen(req, timeout=6) as resp:
                resp.read(2048)
            return
        payload = (title + "\n" + body).encode("utf-8")
        req = urlrequest.Request(url, data=payload, method="POST",
                                 headers={"Title": "AirPrint", "Tags": "printer",
                                          "Content-Type": "text/plain; charset=utf-8"})
        with urlrequest.urlopen(req, timeout=6) as resp:
            resp.read(2048)

    # ---------- CUPS ----------

    def _request(self, op, extra):
        reqid = int(time.time() * 1000) & 0xFFFF
        return (struct.pack(">HHI", 0x0200, op, reqid) +
                b"\x01" +
                common.attr_str("attributes-charset", "utf-8", 0x47) +
                common.attr_str("attributes-natural-language", "en", 0x48) +
                extra + b"\x03")

    def _printer_uri(self):
        return "ipp://%s:%d/printers/%s" % (self.s["cups_host"], self.s["cups_port"], quote(self.s["queue"]))

    def _cups_path(self):
        return "/printers/" + quote(self.s["queue"])

    def _post_cups(self, body, timeout=4):
        return common.http_ipp(self.s["cups_host"], self.s["cups_port"], self._cups_path(), body, timeout)

    def _gpa(self):
        wanted = ("printer-state", "printer-state-reasons", "printer-is-accepting-jobs",
                  "queued-job-count", "printer-state-message", "device-uri",
                  "printer-make-and-model", "printer-uuid")
        body = self._request(OP_GPA,
                             common.attr_str("printer-uri", self._printer_uri(), 0x45) +
                             common.attr_multi("requested-attributes", wanted))
        try:
            resp = self._post_cups(body)
        except OSError as exc:
            self.log.debug("gpa failed: %r", exc)
            return None
        if common.ipp_status(resp) not in (0x0000, 0x0001):
            return None
        attrs = common.parse_attrs(resp)
        return {
            "state": common.attr_number(attrs, "printer-state", 0),
            "reasons": common.attr_list(attrs, "printer-state-reasons"),
            "accepting": common.attr_bool(attrs, "printer-is-accepting-jobs", True),
            "queued": common.attr_number(attrs, "queued-job-count", 0),
            "message": common.attr_text(attrs, "printer-state-message"),
            "device_uri": common.attr_text(attrs, "device-uri"),
            "model": common.attr_text(attrs, "printer-make-and-model"),
            "uuid": common.attr_text(attrs, "printer-uuid"),
        }

    def _get_jobs(self):
        wanted = ("job-id", "job-uri", "job-name", "job-originating-user-name",
                  "job-state", "job-state-reasons", "job-impressions",
                  "job-impressions-completed", "job-media-sheets",
                  "job-media-sheets-completed", "time-at-creation",
                  "time-at-processing")
        body = self._request(OP_GET_JOBS,
                             common.attr_str("printer-uri", self._printer_uri(), 0x45) +
                             common.attr_str("which-jobs", "not-completed", 0x44) +
                             common.attr_multi("requested-attributes", wanted))
        try:
            resp = self._post_cups(body)
        except OSError as exc:
            self.log.debug("get-jobs failed: %r", exc)
            return None
        if common.ipp_status(resp) not in (0x0000, 0x0001):
            return None
        jobs = {}
        for tag, attrs in common.parse_groups(resp):
            if tag != 0x02:
                continue
            jid = common.attr_number(attrs, "job-id", 0)
            if jid:
                jobs[jid] = attrs
        return jobs

    def _get_job(self, jid):
        body = self._request(OP_GJA,
                             common.attr_str("printer-uri", self._printer_uri(), 0x45) +
                             common.attr_int("job-id", jid))
        try:
            resp = self._post_cups(body)
        except OSError:
            return None
        status = common.ipp_status(resp)
        if status not in (0x0000, 0x0001):
            return {}
        return common.parse_attrs(resp)

    # ---------- monitor ----------

    def _monitor_loop(self):
        while not self.stop_event.is_set():
            try:
                self._tick()
            except Exception as exc:
                self.log.warning("monitor error: %r", exc)
            if self.stop_event.wait(3.0):
                break

    def _tick(self):
        now = time.time()
        with self.lock:
            printing = any(j.get("state") == 5 for j in self.active.values())
        if not printing:
            ok, ms = common.tcp_check(self.s["printer"]["host"], self.s["printer"]["port"], 2.0)
            with self.lock:
                was = self.printer["ok"]
                self.printer.update({"ok": ok, "ms": ms, "checked": now})
            if ok and not was:
                self.log.info("printer reachable again (%s ms)", ms)
            with self.lock:
                stale = now - self.printer.get("ready_checked", 0.0) > 20
            if ok and stale:
                ready, _ = common.tls_check(self.s["printer"]["host"], self.s["printer"]["port"], 3.0)
                with self.lock:
                    self.printer.update({"ready": ready, "ready_checked": now})
                if not ready:
                    self.log.info("printer TCP up but TLS/IPP not ready")

        info = self._gpa()
        with self.lock:
            if info is None:
                self.cups.update({"ok": False, "checked": now, "state": None})
            else:
                self.cups.update(info)
                self.cups.update({"ok": True, "checked": now})

        jobs = self._get_jobs()
        seen = set()
        if jobs is not None:
            for jid, attrs in jobs.items():
                seen.add(jid)
                self._absorb(jid, attrs, "monitor")
            with self.lock:
                pending = [jid for jid in self.active if jid not in seen]
            for jid in pending:
                attrs = self._get_job(jid)
                if attrs:
                    state = common.attr_number(attrs, "job-state", 0)
                    if state in (7, 8, 9):
                        self.finalize(jid, state, common.attr_list(attrs, "job-state-reasons"))
                        continue
                    self._absorb(jid, attrs, "monitor")
                    continue
                with self.lock:
                    job = self.active.get(jid)
                    if job:
                        job["missing"] = job.get("missing", 0) + 1
                        missing = job["missing"]
                    else:
                        missing = 0
                if missing >= 2:
                    self.finalize(jid, 9, ["job-removed-from-queue"], missing=True)

        side = read_json(SIDECAR)
        if side:
            with self.lock:
                job = self.active.get(side.get("job"))
                if job:
                    job["side_pages"] = max(job.get("side_pages", 0), int(side.get("pages") or 0))
                    job["side_status"] = side.get("status")
                    job["side_error"] = side.get("error") or ""
            if side.get("status") == "failed":
                with self.lock:
                    job = self.active.get(side.get("job"))
                    if job and not job.get("side_notified"):
                        job["side_notified"] = True
                        name = job.get("name") or "打印任务"
                if job and job.get("side_notified"):
                    self.log.warning("backend failure for job %s: %s", side.get("job"), side.get("error"))
                    self.notify("error", "打印发送失败", '"%s" · %s' % (name, side.get("error") or "打印机连接中断"))

        self._maybe_recover()

    def _absorb(self, jid, attrs, source):
        state = common.attr_number(attrs, "job-state", 0)
        reasons = common.attr_list(attrs, "job-state-reasons")
        if state in (7, 8, 9):
            self.finalize(jid, state, reasons)
            return
        if state not in (3, 4, 5, 6):
            return
        now = time.time()
        created = common.attr_number(attrs, "time-at-creation", 0) or now
        started = common.attr_number(attrs, "time-at-processing", 0) or created
        name = common.attr_text(attrs, "job-name", "")
        user = common.attr_text(attrs, "job-originating-user-name", "")
        announce = source == "client" or (now - created) < 30
        with self.lock:
            job = self.active.get(jid)
            if job is None:
                job = {
                    "id": jid, "uri": common.attr_text(attrs, "job-uri", ""),
                    "name": name, "user": user, "client": "", "source": source,
                    "seen": now, "started": started, "updated": now,
                    "missing": 0, "side_pages": 0, "side_status": "",
                    "side_error": "", "side_notified": False,
                }
                self.active[jid] = job
            else:
                job["missing"] = 0
                job["updated"] = now
                if name:
                    job["name"] = name
                if user:
                    job["user"] = user
                if started and started > 0:
                    job["started"] = started
            job.update({
                "state": state,
                "reasons": reasons,
                "impressions": common.attr_number(attrs, "job-impressions-completed", 0),
                "sheets": common.attr_number(attrs, "job-media-sheets-completed", 0),
                "expected": (common.attr_number(attrs, "job-impressions", 0) or
                             common.attr_number(attrs, "job-media-sheets", 0)),
            })
            uri = common.attr_text(attrs, "job-uri", "")
            if uri:
                job["uri"] = uri
        if announce and source == "monitor":
            self.log.info("job %s discovered: %s (%s)", jid, name or "-", user or "-")

    def job_started(self, jid, name, user, client, state, uri):
        now = time.time()
        with self.lock:
            if jid in self.active:
                return
            self.active[jid] = {
                "id": jid, "uri": uri or "", "name": name or "", "user": user or "",
                "client": client or "", "source": "client",
                "seen": now, "started": now, "updated": now, "missing": 0,
                "side_pages": 0, "side_status": "", "side_error": "",
                "side_notified": False, "state": state or 3, "reasons": [],
                "impressions": 0, "sheets": 0, "expected": 0,
            }
        self.log.info("job %s started: %s (%s from %s)", jid, name or "-", user or "-", client or "-")
        self.notify("start", "开始打印", '"%s"' % (name or "打印任务"))

    def _diagnose(self, state, reasons, missing=False):
        if missing:
            return "任务已从 CUPS 队列移除，通常表示已完成", ""
        if state == 9:
            if any("error" in r or "failed" in r for r in reasons):
                return "打印完成，但队列报告了错误", ",".join(reasons[:3])
            return "打印完成", ""
        if state == 7:
            return "任务已取消", ",".join(reasons[:3])
        base = "任务被系统终止" if state == 8 else "任务已结束"
        logs = common.read_tail(CUPS_LOG, 160, 65536) + "\n" + common.read_tail(BACKEND_LOG, 80, 16384)
        for pattern, hint in DIAG_PATTERNS:
            match = re.search(pattern, logs, re.I)
            if match:
                line = match.group(0)[:120]
                return base + "：" + hint, line
        if reasons:
            return base + "（" + ",".join(reasons[:3]) + "）", ""
        return base, ""

    def finalize(self, jid, state, reasons=None, missing=False):
        now = time.time()
        with self.lock:
            job = self.active.pop(jid, None)
            if job is None:
                return
        reasons = reasons or job.get("reasons") or []
        side = read_json(SIDECAR)
        side_pages = job.get("side_pages") or 0
        if side.get("job") == jid:
            side_pages = max(side_pages, int(side.get("pages") or 0))
        pages = max(job.get("impressions", 0), job.get("sheets", 0), side_pages)
        started = job.get("started") or job.get("seen") or now
        diag, detail = self._diagnose(state, reasons, missing)
        record = {
            "id": jid,
            "name": job.get("name") or "",
            "user": job.get("user") or "",
            "client": job.get("client") or "",
            "start": common.iso(started),
            "end": common.iso(now),
            "duration_s": max(0, int(now - started)),
            "state": state,
            "state_text": STATE_TEXT.get(state, "已结束"),
            "reasons": reasons,
            "pages": pages,
            "expected": job.get("expected") or 0,
            "diagnosis": diag,
            "detail": detail,
        }
        self._append_history(record)
        self.log.info("job %s finished state=%s pages=%s: %s", jid, state, pages, diag)
        name = job.get("name") or "打印任务"
        if state == 9:
            body = '"%s" · %d 页 · 用时 %s' % (name, pages, common.human_duration(now - started))
            self.notify("complete", "打印完成", body)
        elif state == 7:
            self.notify("error", "打印已取消", '"%s"' % name)
        else:
            self.notify("error", "打印失败", '"%s" · %s' % (name, diag))

    def _maybe_recover(self):
        s = self.s
        if not s["auto_recover"]:
            return
        now = time.time()
        with self.lock:
            cups = dict(self.cups)
            printer_ok = self.printer["ok"]
            active = bool(self.active)
        if not printer_ok or not cups.get("ok") or active:
            return
        stopped = cups.get("state") == 5
        closed = cups.get("accepting") is False
        if not stopped and not closed:
            return
        if now - self.last_recover < 60:
            return
        self.last_recover = now
        for cmd in (["/usr/sbin/cupsenable", s["queue"]], ["/usr/sbin/cupsaccept", s["queue"]]):
            try:
                result = subprocess.run(cmd, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, timeout=8)
                if result.returncode != 0:
                    self.log.warning("%s failed: %s", cmd[0], result.stderr.decode("utf-8", "replace")[:200])
            except (OSError, subprocess.TimeoutExpired) as exc:
                self.log.warning("%s failed: %r", cmd[0], exc)
        self.log.info("queue %s auto-recovered (stopped=%s accepting=%s)", s["queue"], stopped, cups.get("accepting"))
        self.notify("recover", "打印队列已恢复", "%s 已重新启用" % s["queue"])

    # ---------- request path ----------

    def touch_client(self, ip, op):
        with self.lock:
            self.last_client = {"ip": ip, "time": time.time(), "op": op}

    def printer_ready(self):
        if not self.s["offline_guard"]:
            return True
        now = time.time()
        with self.lock:
            checked = self.printer.get("ready_checked", 0.0)
            cached = self.printer.get("ready", False)
        if now - checked < 4:
            return cached
        tcp_ok, ms = common.tcp_check(self.s["printer"]["host"], self.s["printer"]["port"], 2.0)
        ready = False
        if tcp_ok:
            ready, _ = common.tls_check(self.s["printer"]["host"], self.s["printer"]["port"], 3.0)
        with self.lock:
            self.printer.update({"ok": tcp_ok, "ready": ready, "ms": ms,
                                 "checked": time.time(), "ready_checked": time.time()})
        if not tcp_ok:
            if now - self.last_offline_notice > 300:
                self.last_offline_notice = now
                self.log.warning("printer offline, blocked job submission")
                self.notify("error", "打印机离线", "已拦截打印任务：无法连接 %s:%d" %
                            (self.s["printer"]["host"], self.s["printer"]["port"]))
        elif not ready:
            if now - self.last_offline_notice > 300:
                self.last_offline_notice = now
                self.log.warning("printer TCP up but TLS/IPP not ready, blocked job submission")
                self.notify("error", "打印机未就绪", "TCP 可连接但 TLS/IPP 未就绪，已延后提交")
        return ready

    # ---------- status ----------

    def snapshot(self):
        now = time.time()
        with self.lock:
            active = sorted(self.active.values(), key=lambda j: j.get("seen", 0), reverse=True)
            history = list(self.history)[-20:][::-1]
            last_client = dict(self.last_client) if self.last_client else None
            printer = dict(self.printer)
            cups = dict(self.cups)
            last_final = self.last_final
        cap = newest_capture()
        side = read_json(SIDECAR)
        avahi_ok = os.path.exists("/var/run/avahi-daemon/pid") and os.path.exists("/etc/avahi/services/airprint.service")
        urf_tail = common.read_tail(URF_LOG, 6, 2048)
        backend_tail = common.read_tail(BACKEND_LOG, 6, 2048)

        stages = []
        if last_client:
            age = int(now - last_client["time"])
            level = "ok" if age < 180 else "idle"
            detail = "%s · %s" % (last_client["ip"], age_text(age))
        else:
            level, detail = "idle", "尚未收到打印请求"
        stages.append({"key": "client", "name": "iPhone 客户端", "level": level, "detail": detail})

        stages.append({
            "key": "mdns", "name": "mDNS 广播", "level": "ok" if avahi_ok else "bad",
            "detail": ("%s · 端口 %d" % (self.s["service_name"], self.s["listen_port"])) if avahi_ok
            else "Avahi 服务或广播文件缺失",
        })

        stages.append({
            "key": "proxy", "name": "AirPrint 代理", "level": "ok",
            "detail": "监听 :%d · 运行 %s" % (self.s["listen_port"], common.human_duration(now - self.started)),
        })

        if not cups.get("ok"):
            cups_level, cups_detail = "bad", "无法连接 CUPS"
        elif cups.get("state") == 5:
            cups_level = "bad"
            cups_detail = "队列已停止 · %s" % (",".join(cups.get("reasons") or []) or "未知原因")
        elif cups.get("accepting") is False:
            cups_level, cups_detail = "warn", "暂停接收新任务"
        else:
            cups_level = "ok"
            cups_detail = {3: "队列空闲", 4: "正在处理", 5: "已停止", 6: "已暂停"}.get(cups.get("state"), "运行中")
            if cups.get("queued"):
                cups_detail += " · %d 个任务" % cups["queued"]
        stages.append({"key": "cups", "name": "CUPS 队列", "level": cups_level, "detail": cups_detail})

        busy = any(j.get("state") == 5 for j in active)
        if busy:
            filt_level, filt_detail = "ok", "正在转换 URF 数据"
        elif cap:
            filt_level = "ok"
            filt_detail = "最近捕获 %s · %d KB" % (age_text(cap["age"]), cap["size"] // 1024)
        else:
            filt_level, filt_detail = "idle", "等待打印数据"
        if "failed" in urf_tail and "capture" in urf_tail:
            filt_level = "warn"
        stages.append({"key": "filter", "name": "URF 转换", "level": filt_level, "detail": filt_detail})

        if side.get("status") == "printing":
            back_level = "ok"
            back_detail = "正在发送第 %d 页" % (int(side.get("pages") or 0))
        elif side.get("status") == "success":
            back_level = "ok"
            back_detail = "上次发送成功 · %d 页" % (int(side.get("pages") or 0))
        elif side.get("status") == "failed":
            back_level = "bad"
            back_detail = "上次发送失败 · %s" % (side.get("error") or "未知错误")[:60]
        else:
            back_level, back_detail = "idle", "暂无发送记录"
        if side.get("status") != "success" and "FAILED" in backend_tail:
            back_level = "bad"
        stages.append({"key": "backend", "name": "IPP 发送", "level": back_level, "detail": back_detail})

        printer_uri = self.s["printer"]
        if not printer.get("ok"):
            printer_level, printer_detail = "bad", "离线或无法连接"
        elif printer.get("ready"):
            printer_level = "ok"
            printer_detail = "%s:%d · %s ms · IPP 就绪" % (
                printer_uri["host"], printer_uri["port"], printer.get("ms"))
        else:
            printer_level, printer_detail = "warn", "%s:%d · TCP 可达但 IPP 未就绪" % (
                printer_uri["host"], printer_uri["port"])
        stages.append({
            "key": "printer", "name": "EPSON L3250",
            "level": printer_level, "detail": printer_detail,
        })

        critical = {s["level"] for s in stages if s["key"] in ("mdns", "proxy", "cups", "printer")}
        levels = {s["level"] for s in stages}
        if "bad" in critical:
            overall = "bad"
        elif "bad" in levels or "warn" in levels:
            overall = "warn"
        else:
            overall = "ok"

        active_out = []
        for job in active:
            pages = max(job.get("impressions", 0), job.get("sheets", 0), job.get("side_pages", 0))
            expected = job.get("expected") or 0
            progress = min(99, int(pages * 100 / expected)) if expected and pages < expected else (100 if expected and pages >= expected else None)
            active_out.append({
                "id": job.get("id"), "name": job.get("name"), "user": job.get("user"),
                "state": job.get("state"), "state_text": STATE_TEXT.get(job.get("state"), "处理中"),
                "reasons": job.get("reasons") or [], "pages": pages, "expected": expected,
                "progress": progress, "elapsed_s": int(now - (job.get("started") or now)),
                "side_status": job.get("side_status") or "",
            })

        return {
            "ok": overall != "bad",
            "level": overall,
            "version": VERSION,
            "time": common.iso(now),
            "stages": stages,
            "active": active_out,
            "queued": int(cups.get("queued") or 0),
            "last": last_final,
            "history": history,
            "system": system_info(self.started),
            "config": {
                "dpi": self.s["dpi"],
                "queue": self.s["queue"],
                "printer_uri": self.s["printer"]["uri"],
                "listen_port": self.s["listen_port"],
                "service_name": self.s["service_name"],
                "uuid": self.s["uuid"],
                "notify": bool(self.s["notify_url"]),
                "notify_kind": self.s["notify_kind"],
                "notify_events": sorted(self.s["notify_events"]),
                "auto_recover": self.s["auto_recover"],
                "offline_guard": self.s["offline_guard"],
                "history_max": self.s["history_max"],
            },
            "logs": {
                "proxy": common.read_tail(LOG_PATH, 8, 4096),
                "backend": backend_tail,
                "urf": urf_tail,
                "cups": common.read_tail(CUPS_LOG, 6, 3072),
            },
        }


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    server_version = "airprint-bridge/" + VERSION
    timeout = 300
    bridge = None

    def log_message(self, fmt, *args):
        pass

    def _send(self, code, body, content_type, cors=False):
        try:
            self.send_response(code)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Connection", "close")
            if cors:
                self.send_header("Access-Control-Allow-Origin", "*")
                self.send_header("Access-Control-Allow-Methods", "GET, OPTIONS")
                self.send_header("Access-Control-Allow-Headers", "Content-Type")
            self.end_headers()
            self.wfile.write(body)
        except OSError:
            pass

    def do_OPTIONS(self):
        path = urlsplit(self.path).path
        if path in ("/status.json", "/healthz"):
            try:
                self.send_response(204)
                self.send_header("Access-Control-Allow-Origin", "*")
                self.send_header("Access-Control-Allow-Methods", "GET, OPTIONS")
                self.send_header("Access-Control-Allow-Headers", "Content-Type")
                self.send_header("Content-Length", "0")
                self.send_header("Connection", "close")
                self.end_headers()
            except OSError:
                pass
        else:
            self._send(404, b"not found\n", "text/plain; charset=utf-8")

    def do_GET(self):
        path = urlsplit(self.path).path
        if path == "/status.json":
            body = json.dumps(self.bridge.snapshot(), ensure_ascii=False).encode("utf-8")
            self._send(200, body, "application/json; charset=utf-8", cors=True)
        elif path == "/healthz":
            self._send(200, b"ok\n", "text/plain; charset=utf-8", cors=True)
        else:
            self._send(404, b"not found\n", "text/plain; charset=utf-8")

    def _read_chunked(self):
        spool = tempfile.SpooledTemporaryFile(max_size=2 * 1024 * 1024, mode="w+b")
        total = 0
        while True:
            line = self.rfile.readline(65536)
            if not line:
                spool.close()
                return None
            try:
                size = int(line.split(b";")[0].strip() or b"0", 16)
            except ValueError:
                spool.close()
                return None
            if size == 0:
                while True:
                    trailer = self.rfile.readline(65536)
                    if trailer in (b"\r\n", b"\n", b""):
                        break
                spool.seek(0)
                return spool, total
            left = size
            while left > 0:
                chunk = self.rfile.read(min(65536, left))
                if not chunk:
                    spool.close()
                    return None
                spool.write(chunk)
                left -= len(chunk)
            self.rfile.read(2)
            total += size
            if total > 64 * 1024 * 1024:
                spool.close()
                return None

    def _drain(self, length):
        left = length
        while left > 0:
            chunk = self.rfile.read(min(65536, left))
            if not chunk:
                return
            left -= len(chunk)

    def _ipp_error(self, first, message):
        reqid = struct.unpack(">I", first[4:8])[0] if len(first) >= 8 else 1
        version = first[:2] if len(first) >= 2 else b"\x02\x00"
        body = (version + struct.pack(">H", 0x0503) + struct.pack(">I", reqid) + b"\x01" +
                common.attr_str("attributes-charset", "utf-8", 0x47) +
                common.attr_str("attributes-natural-language", "zh-cn", 0x48) +
                common.attr_str("status-message", message, 0x41) + b"\x03")
        self._send(200, body, "application/ipp")

    def _forward(self, path, first, stream, left):
        try:
            sock = socket.create_connection((self.bridge.s["cups_host"], self.bridge.s["cups_port"]), 20)
            sock.settimeout(300)
        except OSError as exc:
            self.bridge.log.warning("cups connect failed: %r", exc)
            return None
        try:
            head = ("POST %s HTTP/1.1\r\nHost: %s:%d\r\nContent-Type: application/ipp\r\n"
                    "Content-Length: %d\r\nConnection: close\r\nAccept-Encoding: identity\r\n\r\n"
                    % (path, self.bridge.s["cups_host"], self.bridge.s["cups_port"],
                       len(first) + left)).encode("ascii")
            sock.sendall(head)
            if first:
                sock.sendall(first)
            while left > 0:
                chunk = stream.read(min(65536, left))
                if not chunk:
                    break
                sock.sendall(chunk)
                left -= len(chunk)
            data = b""
            while True:
                chunk = sock.recv(65536)
                if not chunk:
                    break
                data += chunk
                if len(data) > 8 * 1024 * 1024:
                    break
            return data
        except OSError as exc:
            self.bridge.log.warning("cups forward failed: %r", exc)
            return None
        finally:
            try:
                sock.close()
            except OSError:
                pass

    def _handle_ipp(self, path, first, stream, left):
        bridge = self.bridge
        op = struct.unpack(">H", first[2:4])[0]
        ip = self.client_address[0]
        bridge.touch_client(ip, op)
        reqid = struct.unpack(">I", first[4:8])[0]
        if op in SUBMIT_OPS and not bridge.printer_ready():
            self._drain(left)
            bridge.log.info("blocked op=0x%04x client=%s: printer offline", op, ip)
            self._ipp_error(first, "printer offline")
            return
        resp = self._forward(path, first, stream, left)
        if resp is None:
            self._send(502, b"cups unavailable\n", "text/plain; charset=utf-8")
            return
        idx = resp.find(b"\r\n\r\n")
        if idx < 0:
            self._send(502, b"bad cups response\n", "text/plain; charset=utf-8")
            return
        code = 200
        try:
            code = int(resp[:idx].split(b"\r\n")[0].split()[1])
        except (ValueError, IndexError):
            pass
        ipp = resp[idx + 4:]
        status = common.ipp_status(ipp)
        if status in (0x0000, 0x0001):
            host = self.headers.get("Host") or ("192.168.31.1:%d" % bridge.s["listen_port"])
            if ":" not in host:
                host += ":%d" % bridge.s["listen_port"]
            ipp = common.rewrite_uri_attrs(ipp, host)
            if op == OP_GPA:
                ipp = common.enhance_gpa(ipp, bridge.s["urf_values"], bridge.s["airprint_values"])
            elif op == OP_GJA:
                ipp = common.fix_job_reason(ipp)
        if op in (OP_CREATE, OP_PRINT_JOB) and status in (0x0000, 0x0001):
            attrs = common.parse_attrs(ipp)
            jid = common.attr_number(attrs, "job-id", 0)
            if jid:
                req = common.parse_attrs(first)
                bridge.job_started(
                    jid,
                    common.attr_text(req, "job-name"),
                    common.attr_text(req, "requesting-user-name"),
                    ip,
                    common.attr_number(attrs, "job-state", 3),
                    common.attr_text(attrs, "job-uri"),
                )
        bridge.log.debug("op=0x%04x client=%s req=%d ipp=0x%04x resp=%d bytes",
                         op, ip, reqid, status or 0, len(ipp))
        self._send(code, ipp, "application/ipp")

    def do_POST(self):
        bridge = self.bridge
        path = urlsplit(self.path).path or "/"
        transfer = (self.headers.get("Transfer-Encoding") or "").lower()
        if "chunked" in transfer:
            result = self._read_chunked()
            if result is None:
                self._send(400, b"bad chunked body\n", "text/plain; charset=utf-8")
                return
            spool, total = result
            try:
                if total < 8:
                    self._send(400, b"short ipp body\n", "text/plain; charset=utf-8")
                    return
                first = spool.read(65536)
                self._handle_ipp(path, first, spool, total - len(first))
            finally:
                spool.close()
            return
        try:
            length = int(self.headers.get("Content-Length") or 0)
        except ValueError:
            length = 0
        if length < 8:
            self._send(400, b"missing ipp body\n", "text/plain; charset=utf-8")
            return
        first = self.rfile.read(min(65536, length))
        if len(first) < 8:
            self._send(400, b"short ipp body\n", "text/plain; charset=utf-8")
            return
        self._handle_ipp(path, first, self.rfile, length - len(first))


def main():
    settings = load_settings()
    logger = setup_logging(settings["debug"])
    logger.info("airprint-proxy %s starting, queue=%s printer=%s dpi=%d",
                VERSION, settings["queue"], settings["printer"]["uri"], settings["dpi"])
    bridge = Bridge(settings, logger)
    Handler.bridge = bridge
    server = ThreadingHTTPServer(("0.0.0.0", settings["listen_port"]), Handler)
    server.daemon_threads = True

    def stop(signum, frame):
        bridge.stop_event.set()
        threading.Thread(target=server.shutdown, daemon=True).start()

    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    try:
        server.serve_forever(poll_interval=0.5)
    finally:
        server.server_close()
        logger.info("airprint-proxy stopped")
    return 0


if __name__ == "__main__":
    sys.exit(main())
