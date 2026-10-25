package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Stage struct {
	Key    string `json:"key"`
	Name   string `json:"name"`
	Level  string `json:"level"`
	Detail string `json:"detail"`
}

type ActiveJob struct {
	ID         int      `json:"id"`
	Name       string   `json:"name"`
	User       string   `json:"user"`
	State      int      `json:"state"`
	StateText  string   `json:"state_text"`
	Reasons    []string `json:"reasons"`
	Pages      int      `json:"pages"`
	Expected   int      `json:"expected"`
	Progress   *int     `json:"progress"`
	ElapsedS   int      `json:"elapsed_s"`
	SideStatus string   `json:"side_status"`
}

type Snapshot struct {
	OK      bool             `json:"ok"`
	Level   string           `json:"level"`
	Version string           `json:"version"`
	Time    string           `json:"time"`
	Stages  []Stage          `json:"stages"`
	Active  []ActiveJob      `json:"active"`
	Queued  int              `json:"queued"`
	Last    map[string]any   `json:"last"`
	History []map[string]any `json:"history"`
	System  SystemInfo       `json:"system"`
	Config  map[string]any   `json:"config"`
	Logs    map[string]string `json:"logs"`
}

func (b *Bridge) snapshot() Snapshot {
	now := time.Now()
	b.mu.RLock()
	active := make([]*Job, 0, len(b.active))
	for _, j := range b.active {
		cp := *j
		cp.Reasons = append([]string{}, j.Reasons...)
		active = append(active, &cp)
	}
	hist := append([]map[string]any{}, b.history...)
	last := b.lastFinal
	var lastClient *ClientTouch
	if b.lastClient != nil {
		cp := *b.lastClient
		lastClient = &cp
	}
	printer := b.printer
	cups := b.cups
	cups.Reasons = append([]string{}, b.cups.Reasons...)
	b.mu.RUnlock()
	sort.Slice(active, func(i, j int) bool { return active[i].Seen.After(active[j].Seen) })

	if len(hist) > 20 {
		hist = hist[len(hist)-20:]
	}
	history := make([]map[string]any, 0, len(hist))
	for i := len(hist) - 1; i >= 0; i-- {
		history = append(history, hist[i])
	}

	cap := newestCapture(urfDir)
	side := readJSONFile(sidecarPath)
	avahiOK := fileExists("/var/run/avahi-daemon/pid") && fileExists("/etc/avahi/services/airprint.service")
	urfTail := readTail(urfLog, 6, 2048)
	backendTail := readTail(backendLog, 6, 2048)

	stages := []Stage{}
	if lastClient != nil {
		age := int(now.Sub(lastClient.Time).Seconds())
		level := "idle"
		if age < 180 {
			level = "ok"
		}
		stages = append(stages, Stage{"client", "iPhone 客户端", level,
			fmt.Sprintf("%s · %s", lastClient.IP, ageText(age))})
	} else {
		stages = append(stages, Stage{"client", "iPhone 客户端", "idle", "尚未收到打印请求"})
	}

	if avahiOK {
		stages = append(stages, Stage{"mdns", "mDNS 广播", "ok",
			fmt.Sprintf("%s · 端口 %d", b.s.ServiceName, b.s.ListenPort)})
	} else {
		stages = append(stages, Stage{"mdns", "mDNS 广播", "bad", "Avahi 服务或广播文件缺失"})
	}

	stages = append(stages, Stage{"proxy", "AirPrint 代理", "ok",
		fmt.Sprintf("监听 :%d · 运行 %s", b.s.ListenPort, humanDuration(now.Sub(b.started).Seconds()))})

	cupsLevel, cupsDetail := "bad", "无法连接 CUPS"
	if cups.OK {
		switch {
		case cups.State == 5:
			cupsLevel = "bad"
			reason := strings.Join(cups.Reasons, ",")
			if reason == "" {
				reason = "未知原因"
			}
			cupsDetail = "队列已停止 · " + reason
		case cups.Accepting != nil && !*cups.Accepting:
			cupsLevel, cupsDetail = "warn", "暂停接收新任务"
		default:
			cupsLevel = "ok"
			switch cups.State {
			case 3:
				cupsDetail = "队列空闲"
			case 4:
				cupsDetail = "正在处理"
			case 5:
				cupsDetail = "已停止"
			case 6:
				cupsDetail = "已暂停"
			default:
				cupsDetail = "运行中"
			}
			if cups.Queued > 0 {
				cupsDetail += fmt.Sprintf(" · %d 个任务", cups.Queued)
			}
		}
	}
	stages = append(stages, Stage{"cups", "CUPS 队列", cupsLevel, cupsDetail})

	busy := false
	for _, j := range active {
		if j.State == 5 {
			busy = true
			break
		}
	}
	filterLevel, filterDetail := "idle", "等待打印数据"
	if busy {
		filterLevel, filterDetail = "ok", "正在转换 URF 数据"
	} else if cap != nil {
		filterLevel = "ok"
		filterDetail = fmt.Sprintf("最近捕获 %s · %d KB", ageText(cap.Age), cap.Size/1024)
	}
	if strings.Contains(urfTail, "failed") && strings.Contains(urfTail, "capture") {
		filterLevel = "warn"
	}
	stages = append(stages, Stage{"filter", "URF 转换", filterLevel, filterDetail})

	sideStatus := jsonString(side["status"])
	sidePages := jsonInt(side["pages"])
	backLevel, backDetail := "idle", "暂无发送记录"
	switch sideStatus {
	case "printing":
		backLevel, backDetail = "ok", fmt.Sprintf("正在发送第 %d 页", sidePages)
	case "success":
		backLevel, backDetail = "ok", fmt.Sprintf("上次发送成功 · %d 页", sidePages)
	case "failed":
		errText := jsonString(side["error"])
		if errText == "" {
			errText = "未知错误"
		}
		if len([]rune(errText)) > 60 {
			errText = string([]rune(errText)[:60])
		}
		backLevel, backDetail = "bad", "上次发送失败 · "+errText
	}
	if sideStatus != "success" && strings.Contains(backendTail, "FAILED") {
		backLevel = "bad"
	}
	stages = append(stages, Stage{"backend", "IPP 发送", backLevel, backDetail})

	printerLevel := "bad"
	printerDetail := "离线或无法连接"
	if printer.OK {
		if printer.Ready {
			printerLevel = "ok"
			printerDetail = fmt.Sprintf("%s:%d · %d ms · IPP 就绪", b.s.Printer.Host, b.s.Printer.Port, printer.MS)
		} else {
			printerLevel = "warn"
			printerDetail = fmt.Sprintf("%s:%d · TCP 可达但 IPP 未就绪", b.s.Printer.Host, b.s.Printer.Port)
		}
	}
	stages = append(stages, Stage{"printer", "EPSON L3250", printerLevel, printerDetail})

	criticalBad := false
	anyBadOrWarn := false
	for _, s := range stages {
		if s.Level == "bad" {
			anyBadOrWarn = true
			if s.Key == "mdns" || s.Key == "proxy" || s.Key == "cups" || s.Key == "printer" {
				criticalBad = true
			}
		} else if s.Level == "warn" {
			anyBadOrWarn = true
		}
	}
	overall := "ok"
	if criticalBad {
		overall = "bad"
	} else if anyBadOrWarn {
		overall = "warn"
	}

	activeOut := make([]ActiveJob, 0, len(active))
	for _, j := range active {
		pages := j.Impressions
		if j.Sheets > pages {
			pages = j.Sheets
		}
		if j.SidePages > pages {
			pages = j.SidePages
		}
		var progress *int
		if j.Expected > 0 {
			p := 100
			if pages < j.Expected {
				p = pages * 100 / j.Expected
				if p > 99 {
					p = 99
				}
			}
			progress = &p
		}
		elapsed := 0
		if !j.Started.IsZero() {
			elapsed = int(now.Sub(j.Started).Seconds())
		}
		activeOut = append(activeOut, ActiveJob{
			ID: j.ID, Name: j.Name, User: j.User, State: j.State,
			StateText: stateTextOr(j.State), Reasons: j.Reasons,
			Pages: pages, Expected: j.Expected, Progress: progress,
			ElapsedS: elapsed, SideStatus: j.SideStatus,
		})
	}

	events := []string{}
	for k := range b.s.NotifyEvents {
		events = append(events, k)
	}
	sort.Strings(events)

	return Snapshot{
		OK: overall != "bad", Level: overall, Version: version, Time: iso(now),
		Stages: stages, Active: activeOut, Queued: cups.Queued,
		Last: last, History: history,
		System: systemInfo(b.started),
		Config: map[string]any{
			"dpi": b.s.DPI, "queue": b.s.Queue, "printer_uri": b.s.Printer.URI,
			"listen_port": b.s.ListenPort, "service_name": b.s.ServiceName,
			"uuid": b.s.UUID, "notify": b.s.NotifyURL != "",
			"notify_kind": b.s.NotifyKind, "notify_events": events,
			"auto_recover": b.s.AutoRecover, "offline_guard": b.s.OfflineGuard,
			"history_max": b.s.HistoryMax,
		},
		Logs: map[string]string{
			"proxy":   readTail(b.logPath, 8, 4096),
			"backend": backendTail,
			"urf":     urfTail,
			"cups":    readTail(cupsLog, 6, 3072),
		},
	}
}

type proxyServer struct {
	b *Bridge
}

func (p *proxyServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		p.handleGet(w, r)
	case http.MethodOptions:
		p.handleOptions(w, r)
	case http.MethodPost:
		p.handlePost(w, r)
	default:
		sendResponse(w, 404, []byte("not found\n"), "text/plain; charset=utf-8", false)
	}
}

func (p *proxyServer) handleGet(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/status.json":
		body := marshalJSON(p.b.snapshot())
		sendResponse(w, 200, body, "application/json; charset=utf-8", true)
	case "/healthz":
		sendResponse(w, 200, []byte("ok\n"), "text/plain; charset=utf-8", true)
	default:
		sendResponse(w, 404, []byte("not found\n"), "text/plain; charset=utf-8", false)
	}
}

func (p *proxyServer) handleOptions(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/status.json", "/healthz":
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Content-Length", "0")
		h.Set("Connection", "close")
		w.WriteHeader(204)
	default:
		sendResponse(w, 404, []byte("not found\n"), "text/plain; charset=utf-8", false)
	}
}

func sendResponse(w http.ResponseWriter, code int, body []byte, contentType string, cors bool) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Connection", "close")
	if cors {
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
	}
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

func (p *proxyServer) handlePost(w http.ResponseWriter, r *http.Request) {
	data, file, total, err := readRequestBody(r)
	if err != nil {
		sendResponse(w, 400, []byte("bad request body\n"), "text/plain; charset=utf-8", false)
		return
	}
	if file != nil {
		defer func() {
			name := file.Name()
			_ = file.Close()
			_ = os.Remove(name)
		}()
	}
	if total < 8 {
		sendResponse(w, 400, []byte("missing ipp body\n"), "text/plain; charset=utf-8", false)
		return
	}
	var first []byte
	if data != nil {
		first = data
	} else {
		buf := make([]byte, 65536)
		n, _ := file.ReadAt(buf, 0)
		first = buf[:n]
	}
	p.handleIPP(w, r, r.URL.Path, first, data, file, total)
}

func readRequestBody(r *http.Request) ([]byte, *os.File, int64, error) {
	const memLimit = 4 << 20
	const maxBody = 64 << 20
	if r.ContentLength >= 0 && r.ContentLength <= memLimit {
		data, err := io.ReadAll(io.LimitReader(r.Body, memLimit+1))
		if err != nil {
			return nil, nil, 0, err
		}
		return data, nil, int64(len(data)), nil
	}
	_ = ensureDir(runDir, 0o777)
	f, err := os.CreateTemp(runDir, "go-body-*")
	if err != nil {
		return nil, nil, 0, err
	}
	n, err := io.Copy(f, io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, nil, 0, err
	}
	if n > maxBody {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, nil, 0, fmt.Errorf("body too large")
	}
	_, _ = f.Seek(0, io.SeekStart)
	return nil, f, n, nil
}

func (p *proxyServer) handleIPP(w http.ResponseWriter, r *http.Request, path string, first []byte, data []byte, file *os.File, total int64) {
	b := p.b
	if len(first) < 8 {
		sendResponse(w, 400, []byte("short ipp body\n"), "text/plain; charset=utf-8", false)
		return
	}
	op := int(binary.BigEndian.Uint16(first[2:4]))
	clientIP := r.RemoteAddr
	if host, _, err := net.SplitHostPort(clientIP); err == nil {
		clientIP = host
	}
	b.touchClient(clientIP, op)
	if (op == opPrintJob || op == opValidate || op == opCreate) && !b.printerReady() {
		b.log.Info("blocked op=0x%04x client=%s: printer offline", op, clientIP)
		body := ippError(first, "printer offline")
		sendResponse(w, 200, body, "application/ipp", false)
		return
	}
	resp, code, err := b.forwardIPP(path, data, file, total)
	if err != nil {
		b.log.Warning("cups forward failed: %v", err)
		sendResponse(w, 502, []byte("cups unavailable\n"), "text/plain; charset=utf-8", false)
		return
	}
	ippBody := resp
	status := ippStatus(ippBody)
	if status == 0 || status == 1 {
		host := r.Host
		if !strings.Contains(host, ":") {
			host = fmt.Sprintf("%s:%d", host, b.s.ListenPort)
		}
		ippBody = rewriteURIAttrs(ippBody, host)
		if op == opGPA {
			ippBody = enhanceGPA(ippBody, b.s.URFValues, b.s.AirprintValues)
		} else if op == opGJA {
			ippBody = fixJobReason(ippBody)
		}
	}
	if (op == opCreate || op == opPrintJob) && (status == 0 || status == 1) {
		attrs := parseAttrs(ippBody)
		jid := attrNumber(attrs, "job-id", 0)
		if jid > 0 {
			req := parseAttrs(first)
			b.jobStarted(jid,
				attrText(req, "job-name", ""),
				attrText(req, "requesting-user-name", ""),
				clientIP,
				attrNumber(attrs, "job-state", 3),
				attrText(attrs, "job-uri", ""))
		}
	}
	b.log.Debug("op=0x%04x client=%s ipp=0x%04x resp=%d bytes", op, clientIP, status, len(ippBody))
	sendResponse(w, code, ippBody, "application/ipp", false)
}

func (b *Bridge) forwardIPP(path string, data []byte, file *os.File, total int64) ([]byte, int, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(b.s.CupsHost, strconv.Itoa(b.s.CupsPort)), 20*time.Second)
	if err != nil {
		return nil, 0, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(300 * time.Second))
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s:%d\r\nContent-Type: application/ipp\r\n"+
		"Content-Length: %d\r\nConnection: close\r\nAccept-Encoding: identity\r\n\r\n",
		path, b.s.CupsHost, b.s.CupsPort, total)
	if _, err := conn.Write([]byte(head)); err != nil {
		return nil, 0, err
	}
	if data != nil {
		if _, err := conn.Write(data); err != nil {
			return nil, 0, err
		}
	} else if _, err := io.Copy(conn, file); err != nil {
		return nil, 0, err
	}
	raw, err := io.ReadAll(io.LimitReader(conn, 8<<20))
	if err != nil {
		return nil, 0, err
	}
	idx := strings.Index(string(raw), "\r\n\r\n")
	if idx < 0 {
		return nil, 0, fmt.Errorf("bad cups response")
	}
	code := 200
	line := strings.SplitN(string(raw[:idx]), "\r\n", 2)[0]
	fields := strings.Fields(line)
	if len(fields) >= 2 {
		if n, err := strconv.Atoi(fields[1]); err == nil {
			code = n
		}
	}
	return raw[idx+4:], code, nil
}
