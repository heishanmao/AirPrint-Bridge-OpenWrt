package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime/debug"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	runDir      = "/tmp/airprint"
	urfDir      = runDir + "/urf"
	backendLog  = runDir + "/backend.log"
	urfLog      = runDir + "/urf.log"
	sidecarPath = runDir + "/backend.json"
	cupsLog     = "/var/log/cups/error_log"
)

var stateText = map[int]string{
	3: "等待中", 4: "已保留", 5: "打印中", 6: "已暂停",
	7: "已取消", 8: "已中止", 9: "已完成",
}

var diagPatterns = []struct {
	re   *regexp.Regexp
	hint string
}{
	{regexp.MustCompile(`(?i)Filter failed`), "过滤链失败：URF 转换或发送中断（打印机离线或忙）"},
	{regexp.MustCompile(`(?i)rastertopwg (rc=|failed)`), "PWG 栅格转换失败"},
	{regexp.MustCompile(`(?i)raster data truncated|short PWG file`), "打印数据不完整"},
	{regexp.MustCompile(`(?i)Unable to connect|Connection refused|connection refused|timed out|handshake|TLS|SSL`), "无法连接打印机 IPP"},
	{regexp.MustCompile(`(?i)No space left`), "存储空间不足"},
	{regexp.MustCompile(`(?i)no pages in job|No pages`), "任务没有可打印页面"},
	{regexp.MustCompile(`(?i)job-canceled|Job canceled`), "任务被取消"},
}

type ClientTouch struct {
	IP   string
	Time time.Time
	Op   int
}

type PrinterState struct {
	OK           bool
	Ready        bool
	MS           int
	Checked      time.Time
	ReadyChecked time.Time
}

type CupsState struct {
	OK        bool
	State     int
	Reasons   []string
	Accepting *bool
	Queued    int
	Message   string
	DeviceURI string
	Model     string
	UUID      string
	Checked   time.Time
}

type Job struct {
	ID           int
	URI          string
	Name         string
	User         string
	Client       string
	Source       string
	Seen         time.Time
	Started      time.Time
	Updated      time.Time
	Missing      int
	SidePages    int
	SideStatus   string
	SideError    string
	SideNotified bool
	State        int
	Reasons      []string
	Impressions  int
	Sheets       int
	Expected     int
}

type notifyMsg struct {
	Title string
	Body  string
}

type Bridge struct {
	s    Settings
	log  *Logger
	mu   sync.RWMutex
	stop chan struct{}

	started           time.Time
	lastClient        *ClientTouch
	printer           PrinterState
	cups              CupsState
	active            map[int]*Job
	history           []map[string]any
	lastFinal         map[string]any
	lastRecover       time.Time
	lastOfflineNotice time.Time
	notifyQ           chan notifyMsg
	stopOnce          sync.Once

	noNotify  bool
	noActions bool
	noGuard   bool
	historyPath string
	logPath   string
}

func newBridge(s Settings, logger *Logger, historyPath, logPath string, noNotify, noActions, noGuard bool) *Bridge {
	b := &Bridge{
		s: s, log: logger, stop: make(chan struct{}),
		started: time.Now(), active: map[int]*Job{},
		notifyQ: make(chan notifyMsg, 32),
		noNotify: noNotify, noActions: noActions, noGuard: noGuard,
		historyPath: historyPath, logPath: logPath,
	}
	_ = ensureDir(runDir, 0o777)
	_ = ensureDir(urfDir, 0o777)
	b.loadHistory()
	go b.monitorLoop()
	go b.notifyLoop()
	return b
}

func (b *Bridge) Stop() {
	b.stopOnce.Do(func() { close(b.stop) })
}

func (b *Bridge) loadHistory() {
	data, err := os.ReadFile(b.historyPath)
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	total := len(lines)
	if len(lines) > b.s.HistoryMax {
		lines = lines[len(lines)-b.s.HistoryMax:]
	}
	records := []map[string]any{}
	for _, line := range lines {
		rec := map[string]any{}
		if json.Unmarshal([]byte(line), &rec) == nil {
			records = append(records, rec)
		}
	}
	b.mu.Lock()
	b.history = records
	if len(records) > 0 {
		b.lastFinal = records[len(records)-1]
	}
	b.mu.Unlock()
	if total > b.s.HistoryMax {
		b.rewriteHistory()
	}
}

func (b *Bridge) rewriteHistory() {
	tmp := b.historyPath + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return
	}
	b.mu.RLock()
	records := append([]map[string]any{}, b.history...)
	b.mu.RUnlock()
	start := 0
	if len(records) > b.s.HistoryMax {
		start = len(records) - b.s.HistoryMax
	}
	for _, rec := range records[start:] {
		_, _ = f.Write(append(marshalJSON(rec), '\n'))
	}
	_ = f.Close()
	if err := os.Rename(tmp, b.historyPath); err != nil {
		b.log.Warning("history rewrite failed: %v", err)
	}
}

func (b *Bridge) appendHistory(record map[string]any) {
	b.mu.Lock()
	b.history = append(b.history, record)
	over := len(b.history) > b.s.HistoryMax
	if over {
		b.history = b.history[len(b.history)-b.s.HistoryMax:]
	}
	b.lastFinal = record
	b.mu.Unlock()
	if err := ensureDir(filepath.Dir(b.historyPath), 0o755); err == nil {
		if f, err := os.OpenFile(b.historyPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			_, _ = f.Write(append(marshalJSON(record), '\n'))
			_ = f.Close()
		} else {
			b.log.Warning("history append failed: %v", err)
		}
	}
	if over {
		b.rewriteHistory()
	}
}

func (b *Bridge) notify(event, title, body string) {
	if b.noNotify || b.s.NotifyURL == "" || !b.s.NotifyEvents[event] {
		return
	}
	select {
	case b.notifyQ <- notifyMsg{Title: title, Body: body}:
	default:
	}
}

func (b *Bridge) notifyLoop() {
	client := &http.Client{Timeout: 6 * time.Second}
	for {
		select {
		case <-b.stop:
			return
		case msg := <-b.notifyQ:
			if err := b.sendNotify(client, msg.Title, msg.Body); err != nil {
				b.log.Warning("notify failed: %v", err)
			}
		}
	}
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) > n {
		return string(r[:n])
	}
	return s
}

func (b *Bridge) sendNotify(client *http.Client, title, body string) error {
	title = truncateRunes(title, 64)
	body = truncateRunes(body, 300)
	kind := b.s.NotifyKind
	if kind == "auto" {
		if strings.Contains(b.s.NotifyURL, "api.day.app") {
			kind = "bark"
		} else {
			kind = "ntfy"
		}
	}
	switch kind {
	case "bark":
		target := strings.TrimRight(b.s.NotifyURL, "/") + "/" + url.PathEscape(title) + "/" + url.PathEscape(body) + "?group=airprint"
		resp, err := client.Get(target)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return nil
	case "generic":
		payload := marshalJSON(map[string]string{"title": title, "body": body})
		req, _ := http.NewRequest(http.MethodPost, b.s.NotifyURL, strings.NewReader(string(payload)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return nil
	default:
		req, _ := http.NewRequest(http.MethodPost, b.s.NotifyURL, strings.NewReader(title+"\n"+body))
		req.Header.Set("Title", "AirPrint")
		req.Header.Set("Tags", "printer")
		req.Header.Set("Content-Type", "text/plain; charset=utf-8")
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
		return nil
	}
}

func (b *Bridge) request(op int, extra []byte) []byte {
	reqid := uint32(time.Now().UnixMilli()) & 0xFFFF
	out := []byte{0x02, 0x00, byte(op >> 8), byte(op)}
	id := make([]byte, 4)
	binary.BigEndian.PutUint32(id, reqid)
	out = append(out, id...)
	out = append(out, 0x01)
	out = append(out, attrStr(0x47, "attributes-charset", "utf-8")...)
	out = append(out, attrStr(0x48, "attributes-natural-language", "en")...)
	out = append(out, extra...)
	out = append(out, 0x03)
	return out
}

func (b *Bridge) printerURI() string {
	return fmt.Sprintf("ipp://%s:%d/printers/%s", b.s.CupsHost, b.s.CupsPort, url.PathEscape(b.s.Queue))
}

func (b *Bridge) cupsPath() string {
	return "/printers/" + url.PathEscape(b.s.Queue)
}

func (b *Bridge) postCups(body []byte, timeout time.Duration) ([]byte, error) {
	return httpIPP(b.s.CupsHost, b.s.CupsPort, b.cupsPath(), body, timeout)
}

func (b *Bridge) gpa() *CupsState {
	wanted := []string{"printer-state", "printer-state-reasons", "printer-is-accepting-jobs",
		"queued-job-count", "printer-state-message", "device-uri", "printer-make-and-model", "printer-uuid"}
	body := b.request(opGPA, append(
		attrStr(0x45, "printer-uri", b.printerURI()),
		attrMulti(0x44, "requested-attributes", wanted)...))
	resp, err := b.postCups(body, 4*time.Second)
	if err != nil {
		b.log.Debug("gpa failed: %v", err)
		return nil
	}
	status := ippStatus(resp)
	if status != 0 && status != 1 {
		return nil
	}
	attrs := parseAttrs(resp)
	accepting := attrBool(attrs, "printer-is-accepting-jobs", true)
	return &CupsState{
		OK: true, State: attrNumber(attrs, "printer-state", 0),
		Reasons: attrList(attrs, "printer-state-reasons"), Accepting: &accepting,
		Queued: attrNumber(attrs, "queued-job-count", 0),
		Message: attrText(attrs, "printer-state-message", ""),
		DeviceURI: attrText(attrs, "device-uri", ""),
		Model: attrText(attrs, "printer-make-and-model", ""),
		UUID:  attrText(attrs, "printer-uuid", ""),
	}
}

func (b *Bridge) getJobs() map[int]map[string][]ippValue {
	wanted := []string{"job-id", "job-uri", "job-name", "job-originating-user-name",
		"job-state", "job-state-reasons", "job-impressions", "job-impressions-completed",
		"job-media-sheets", "job-media-sheets-completed", "time-at-creation", "time-at-processing"}
	body := b.request(opGetJobs, append(
		append(attrStr(0x45, "printer-uri", b.printerURI()),
			attrStr(0x44, "which-jobs", "not-completed")...),
		attrMulti(0x44, "requested-attributes", wanted)...))
	resp, err := b.postCups(body, 4*time.Second)
	if err != nil {
		b.log.Debug("get-jobs failed: %v", err)
		return nil
	}
	status := ippStatus(resp)
	if status != 0 && status != 1 {
		return nil
	}
	jobs := map[int]map[string][]ippValue{}
	for _, g := range parseGroups(resp) {
		if g.tag != 0x02 {
			continue
		}
		jid := attrNumber(g.attrs, "job-id", 0)
		if jid != 0 {
			jobs[jid] = g.attrs
		}
	}
	return jobs
}

func (b *Bridge) getJob(jid int) map[string][]ippValue {
	body := b.request(opGJA, append(
		attrStr(0x45, "printer-uri", b.printerURI()),
		attrInt("job-id", int32(jid))...))
	resp, err := b.postCups(body, 4*time.Second)
	if err != nil {
		return nil
	}
	status := ippStatus(resp)
	if status != 0 && status != 1 {
		return nil
	}
	return parseAttrs(resp)
}

func (b *Bridge) monitorLoop() {
	for {
		b.tick()
		select {
		case <-b.stop:
			return
		case <-time.After(3 * time.Second):
		}
	}
}

func (b *Bridge) tick() {
	now := time.Now()
	b.mu.RLock()
	printing := false
	for _, j := range b.active {
		if j.State == 5 {
			printing = true
			break
		}
	}
	b.mu.RUnlock()
	if !printing {
		ok, ms := tcpCheck(b.s.Printer.Host, b.s.Printer.Port, 2*time.Second)
		b.mu.Lock()
		was := b.printer.OK
		b.printer.OK = ok
		b.printer.MS = ms
		b.printer.Checked = now
		stale := now.Sub(b.printer.ReadyChecked) > 20*time.Second
		b.mu.Unlock()
		if ok && !was {
			b.log.Info("printer reachable again (%d ms)", ms)
		}
		if ok && stale {
			ready := tlsCheck(b.s.Printer.Host, b.s.Printer.Port, 3*time.Second)
			b.mu.Lock()
			b.printer.Ready = ready
			b.printer.ReadyChecked = now
			b.mu.Unlock()
			if !ready {
				b.log.Info("printer TCP up but TLS/IPP not ready")
			}
		}
	}
	info := b.gpa()
	b.mu.Lock()
	if info == nil {
		b.cups.OK = false
		b.cups.State = 0
		b.cups.Checked = now
	} else {
		info.Checked = now
		b.cups = *info
	}
	b.mu.Unlock()
	jobs := b.getJobs()
	seen := map[int]bool{}
	if jobs != nil {
		for jid, attrs := range jobs {
			seen[jid] = true
			b.absorb(jid, attrs, "monitor")
		}
		b.mu.RLock()
		pending := []int{}
		for jid := range b.active {
			if !seen[jid] {
				pending = append(pending, jid)
			}
		}
		b.mu.RUnlock()
		for _, jid := range pending {
			attrs := b.getJob(jid)
			if attrs != nil {
				state := attrNumber(attrs, "job-state", 0)
				if state == 7 || state == 8 || state == 9 {
					b.finalize(jid, state, attrList(attrs, "job-state-reasons"), false)
					continue
				}
				b.absorb(jid, attrs, "monitor")
				continue
			}
			b.mu.Lock()
			job := b.active[jid]
			missing := 0
			if job != nil {
				job.Missing++
				missing = job.Missing
			}
			b.mu.Unlock()
			if missing >= 2 {
				b.finalize(jid, 9, []string{"job-removed-from-queue"}, true)
			}
		}
	}
	side := readJSONFile(sidecarPath)
	if len(side) > 0 {
		jid := jsonInt(side["job"])
		pages := jsonInt(side["pages"])
		status := jsonString(side["status"])
		errText := jsonString(side["error"])
		b.mu.Lock()
		job := b.active[jid]
		if job != nil {
			if pages > job.SidePages {
				job.SidePages = pages
			}
			job.SideStatus = status
			job.SideError = errText
		}
		b.mu.Unlock()
		if status == "failed" {
			name := "打印任务"
			notified := false
			b.mu.Lock()
			job = b.active[jid]
			if job != nil && !job.SideNotified {
				job.SideNotified = true
				if job.Name != "" {
					name = job.Name
				}
				notified = true
			}
			b.mu.Unlock()
			if notified {
				b.log.Warning("backend failure for job %d: %s", jid, errText)
				msg := errText
				if msg == "" {
					msg = "打印机连接中断"
				}
				b.notify("error", "打印发送失败", "\""+name+"\" · "+msg)
			}
		}
	}
	b.maybeRecover()
}

func (b *Bridge) absorb(jid int, attrs map[string][]ippValue, source string) {
	state := attrNumber(attrs, "job-state", 0)
	reasons := attrList(attrs, "job-state-reasons")
	if state == 7 || state == 8 || state == 9 {
		b.finalize(jid, state, reasons, false)
		return
	}
	if state != 3 && state != 4 && state != 5 && state != 6 {
		return
	}
	now := time.Now()
	created := int64(attrNumber(attrs, "time-at-creation", 0))
	if created == 0 {
		created = now.Unix()
	}
	started := int64(attrNumber(attrs, "time-at-processing", 0))
	if started == 0 {
		started = created
	}
	name := attrText(attrs, "job-name", "")
	user := attrText(attrs, "job-originating-user-name", "")
	announce := source == "client" || now.Unix()-created < 30
	b.mu.Lock()
	job := b.active[jid]
	isNew := false
	if job == nil {
		job = &Job{ID: jid, Name: name, User: user, Source: source,
			Seen: now, Started: time.Unix(started, 0), Updated: now}
		b.active[jid] = job
		isNew = true
	} else {
		job.Missing = 0
		job.Updated = now
		if name != "" {
			job.Name = name
		}
		if user != "" {
			job.User = user
		}
		if started > 0 {
			job.Started = time.Unix(started, 0)
		}
	}
	job.State = state
	job.Reasons = reasons
	job.Impressions = attrNumber(attrs, "job-impressions-completed", 0)
	job.Sheets = attrNumber(attrs, "job-media-sheets-completed", 0)
	job.Expected = attrNumber(attrs, "job-impressions", 0)
	if job.Expected == 0 {
		job.Expected = attrNumber(attrs, "job-media-sheets", 0)
	}
	if uri := attrText(attrs, "job-uri", ""); uri != "" {
		job.URI = uri
	}
	b.mu.Unlock()
	if announce && source == "monitor" && isNew {
		b.log.Info("job %d discovered: %s (%s)", jid, orDash(name), orDash(user))
	}
}

func (b *Bridge) jobStarted(jid int, name, user, client string, state int, uri string) {
	now := time.Now()
	b.mu.Lock()
	if _, exists := b.active[jid]; exists {
		b.mu.Unlock()
		return
	}
	job := &Job{ID: jid, URI: uri, Name: name, User: user, Client: client,
		Source: "client", Seen: now, Started: now, Updated: now, State: state}
	if job.State == 0 {
		job.State = 3
	}
	b.active[jid] = job
	b.mu.Unlock()
	b.log.Info("job %d started: %s (%s from %s)", jid, orDash(name), orDash(user), orDash(client))
	b.notify("start", "开始打印", "\""+firstNonEmpty(name, "打印任务")+"\"")
}

func (b *Bridge) diagnose(state int, reasons []string, missing bool) (string, string) {
	if missing {
		return "任务已从 CUPS 队列移除，通常表示已完成", ""
	}
	if state == 9 {
		hasErr := false
		for _, r := range reasons {
			if strings.Contains(r, "error") || strings.Contains(r, "failed") {
				hasErr = true
				break
			}
		}
		if hasErr {
			return "打印完成，但队列报告了错误", strings.Join(firstN(reasons, 3), ",")
		}
		return "打印完成", ""
	}
	if state == 7 {
		return "任务已取消", strings.Join(firstN(reasons, 3), ",")
	}
	base := "任务已结束"
	if state == 8 {
		base = "任务被系统终止"
	}
	logs := readTail(cupsLog, 160, 65536) + "\n" + readTail(backendLog, 80, 16384)
	for _, p := range diagPatterns {
		if m := p.re.FindString(logs); m != "" {
			if len(m) > 120 {
				m = m[:120]
			}
			return base + "：" + p.hint, m
		}
	}
	if len(reasons) > 0 {
		return base + "（" + strings.Join(firstN(reasons, 3), ",") + "）", ""
	}
	return base, ""
}

func (b *Bridge) finalize(jid, state int, reasons []string, missing bool) {
	now := time.Now()
	b.mu.Lock()
	job := b.active[jid]
	if job == nil {
		b.mu.Unlock()
		return
	}
	delete(b.active, jid)
	b.mu.Unlock()
	if reasons == nil {
		reasons = job.Reasons
	}
	side := readJSONFile(sidecarPath)
	sidePages := job.SidePages
	if jsonInt(side["job"]) == jid {
		if p := jsonInt(side["pages"]); p > sidePages {
			sidePages = p
		}
	}
	pages := job.Impressions
	if job.Sheets > pages {
		pages = job.Sheets
	}
	if sidePages > pages {
		pages = sidePages
	}
	started := job.Started
	if started.IsZero() {
		started = job.Seen
	}
	if started.IsZero() {
		started = now
	}
	diag, detail := b.diagnose(state, reasons, missing)
	record := map[string]any{
		"id": jid, "name": job.Name, "user": job.User, "client": job.Client,
		"start": iso(started), "end": iso(now),
		"duration_s": int(now.Sub(started).Seconds()),
		"state":      state, "state_text": stateTextOr(state),
		"reasons": reasons, "pages": pages, "expected": job.Expected,
		"diagnosis": diag, "detail": detail,
	}
	b.appendHistory(record)
	b.log.Info("job %d finished state=%d pages=%d: %s", jid, state, pages, diag)
	name := firstNonEmpty(job.Name, "打印任务")
	if state == 9 {
		b.notify("complete", "打印完成", fmt.Sprintf("\"%s\" · %d 页 · 用时 %s", name, pages, humanDuration(now.Sub(started).Seconds())))
	} else if state == 7 {
		b.notify("error", "打印已取消", "\""+name+"\"")
	} else {
		b.notify("error", "打印失败", "\""+name+"\" · "+diag)
	}
	debug.FreeOSMemory()
}

func (b *Bridge) maybeRecover() {
	if !b.s.AutoRecover || b.noActions {
		return
	}
	now := time.Now()
	b.mu.RLock()
	printerOK := b.printer.OK
	cups := b.cups
	active := len(b.active) > 0
	b.mu.RUnlock()
	if !printerOK || !cups.OK || active {
		return
	}
	stopped := cups.State == 5
	closed := cups.Accepting != nil && !*cups.Accepting
	if !stopped && !closed {
		return
	}
	b.mu.Lock()
	if now.Sub(b.lastRecover) < 60*time.Second {
		b.mu.Unlock()
		return
	}
	b.lastRecover = now
	b.mu.Unlock()
	for _, cmd := range [][]string{{"/usr/sbin/cupsenable", b.s.Queue}, {"/usr/sbin/cupsaccept", b.s.Queue}} {
		if out, err := exec.Command(cmd[0], cmd[1:]...).CombinedOutput(); err != nil {
			b.log.Warning("%s failed: %s", cmd[0], strings.TrimSpace(string(out)))
		}
	}
	b.log.Info("queue %s auto-recovered (stopped=%v accepting=%v)", b.s.Queue, stopped, cups.Accepting != nil && *cups.Accepting)
	b.notify("recover", "打印队列已恢复", b.s.Queue+" 已重新启用")
}

func (b *Bridge) printerReady() bool {
	if b.noGuard || !b.s.OfflineGuard {
		return true
	}
	now := time.Now()
	b.mu.RLock()
	checked := b.printer.ReadyChecked
	cached := b.printer.Ready
	b.mu.RUnlock()
	if now.Sub(checked) < 4*time.Second {
		return cached
	}
	tcpOK, ms := tcpCheck(b.s.Printer.Host, b.s.Printer.Port, 2*time.Second)
	ready := false
	if tcpOK {
		ready = tlsCheck(b.s.Printer.Host, b.s.Printer.Port, 3*time.Second)
	}
	notifyNow := false
	b.mu.Lock()
	b.printer.OK = tcpOK
	b.printer.Ready = ready
	b.printer.MS = ms
	b.printer.Checked = time.Now()
	b.printer.ReadyChecked = time.Now()
	if !tcpOK || !ready {
		if now.Sub(b.lastOfflineNotice) > 5*time.Minute {
			b.lastOfflineNotice = now
			notifyNow = true
		}
	}
	b.mu.Unlock()
	if notifyNow {
		if !tcpOK {
			b.log.Warning("printer offline, blocked job submission")
			b.notify("error", "打印机离线", fmt.Sprintf("已拦截打印任务：无法连接 %s:%d", b.s.Printer.Host, b.s.Printer.Port))
		} else {
			b.log.Warning("printer TCP up but TLS/IPP not ready, blocked job submission")
			b.notify("error", "打印机未就绪", "TCP 可连接但 TLS/IPP 未就绪，已延后提交")
		}
	}
	return ready
}

func (b *Bridge) touchClient(ip string, op int) {
	b.mu.Lock()
	b.lastClient = &ClientTouch{IP: ip, Time: time.Now(), Op: op}
	b.mu.Unlock()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func stateTextOr(state int) string {
	if s, ok := stateText[state]; ok {
		return s
	}
	return "已结束"
}

func firstN(list []string, n int) []string {
	if len(list) > n {
		return list[:n]
	}
	return list
}

func jsonInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		i, _ := strconv.Atoi(n)
		return i
	}
	return 0
}

func jsonString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}
