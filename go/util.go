package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Logger struct {
	mu    sync.Mutex
	path  string
	file  *os.File
	debug bool
}

var localZone = time.FixedZone("CST", 8*3600)

func newLogger(path string, debug bool) *Logger {
	l := &Logger{path: path, debug: debug}
	_ = ensureDir(filepath.Dir(path), 0o755)
	l.rotateIfNeeded()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		l.file = f
	}
	return l
}

func (l *Logger) rotateIfNeeded() {
	st, err := os.Stat(l.path)
	if err != nil || st.Size() < 262144 {
		return
	}
	_ = os.Rename(l.path, l.path+".1")
}

func (l *Logger) write(level, format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.rotateIfNeeded()
	line := time.Now().In(localZone).Format("2006-01-02 15:04:05") + " " + level + " " +
		fmt.Sprintf(format, args...) + "\n"
	if l.file == nil {
		if f, err := os.OpenFile(l.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			l.file = f
		}
	}
	if l.file != nil {
		_, _ = l.file.WriteString(line)
	}
}

func (l *Logger) Info(format string, args ...any)    { l.write("INFO", format, args...) }
func (l *Logger) Warning(format string, args ...any) { l.write("WARNING", format, args...) }
func (l *Logger) Debug(format string, args ...any) {
	if l.debug {
		l.write("DEBUG", format, args...)
	}
}

func (l *Logger) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		_ = l.file.Close()
		l.file = nil
	}
}

func ensureDir(path string, mode os.FileMode) error {
	if path == "" || path == "." {
		return nil
	}
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	_ = os.Chmod(path, mode)
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func iso(ts time.Time) string {
	return ts.In(localZone).Format("2006-01-02T15:04:05")
}

func humanDuration(seconds float64) string {
	s := int(seconds)
	if s < 0 {
		s = 0
	}
	if s < 60 {
		return fmt.Sprintf("%ds", s)
	}
	if s < 3600 {
		return fmt.Sprintf("%dm%02ds", s/60, s%60)
	}
	return fmt.Sprintf("%dh%02dm", s/3600, (s%3600)/60)
}

func ageText(seconds int) string {
	if seconds < 0 {
		seconds = 0
	}
	if seconds < 60 {
		return fmt.Sprintf("%d 秒前", seconds)
	}
	if seconds < 3600 {
		return fmt.Sprintf("%d 分钟前", seconds/60)
	}
	return fmt.Sprintf("%d 小时前", seconds/3600)
}

func readTail(path string, lines int, maxBytes int64) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	offset := st.Size() - maxBytes
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, 0); err != nil {
		return ""
	}
	data := make([]byte, st.Size()-offset)
	n, _ := f.Read(data)
	data = data[:n]
	parts := strings.Split(string(data), "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func readJSONFile(path string) map[string]any {
	data, err := os.ReadFile(path)
	if err != nil {
		return map[string]any{}
	}
	out := map[string]any{}
	if json.Unmarshal(data, &out) != nil {
		return map[string]any{}
	}
	return out
}

func marshalJSON(value any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(value)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func tcpCheck(host string, port int, timeout time.Duration) (bool, int) {
	started := time.Now()
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false, 0
	}
	_ = conn.Close()
	return true, int(time.Since(started) / time.Millisecond)
}

func tlsCheck(host string, port int, timeout time.Duration) bool {
	dialer := &net.Dialer{Timeout: timeout}
	conn, err := tls.DialWithDialer(dialer, "tcp", net.JoinHostPort(host, strconv.Itoa(port)), &tls.Config{
		InsecureSkipVerify: true,
		ServerName:         host,
	})
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

type Capture struct {
	Path string `json:"path"`
	Age  int    `json:"age"`
	Size int64  `json:"size"`
}

func newestCapture(dir string) *Capture {
	names, err := filepath.Glob(filepath.Join(dir, "raw-*.urf"))
	if err != nil || len(names) == 0 {
		return nil
	}
	newest := ""
	var newestTime time.Time
	for _, name := range names {
		st, err := os.Stat(name)
		if err != nil {
			continue
		}
		if newest == "" || st.ModTime().After(newestTime) {
			newest, newestTime = name, st.ModTime()
		}
	}
	if newest == "" {
		return nil
	}
	st, _ := os.Stat(newest)
	size := int64(0)
	if st != nil {
		size = st.Size()
	}
	return &Capture{Path: newest, Age: int(time.Since(newestTime).Seconds()), Size: size}
}

type SystemInfo struct {
	UptimeS    int     `json:"uptime_s"`
	UptimeText string  `json:"uptime_text"`
	MemTotalMB int     `json:"mem_total_mb"`
	MemAvailMB int     `json:"mem_avail_mb"`
	Load1      float64 `json:"load1"`
	DiskFreeMB int     `json:"disk_free_mb"`
}

func systemInfo(started time.Time) SystemInfo {
	totalKB, availKB := 0, 0
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(line, "MemTotal:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					totalKB, _ = strconv.Atoi(fields[1])
				}
			} else if strings.HasPrefix(line, "MemAvailable:") {
				fields := strings.Fields(line)
				if len(fields) >= 2 {
					availKB, _ = strconv.Atoi(fields[1])
				}
			}
		}
	}
	load1 := 0.0
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		if fields := strings.Fields(string(data)); len(fields) > 0 {
			load1, _ = strconv.ParseFloat(fields[0], 64)
		}
	}
	diskFree := 0
	var st syscall.Statfs_t
	if syscall.Statfs("/overlay", &st) == nil {
		diskFree = int(st.Bavail * uint64(st.Bsize) / (1024 * 1024))
	}
	uptime := time.Since(started).Seconds()
	return SystemInfo{
		UptimeS:    int(uptime),
		UptimeText: humanDuration(uptime),
		MemTotalMB: totalKB / 1024,
		MemAvailMB: availKB / 1024,
		Load1:      load1,
		DiskFreeMB: diskFree,
	}
}
