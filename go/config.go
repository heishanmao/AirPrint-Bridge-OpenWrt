package main

import (
	"net/url"
	"os"
	"strconv"
	"strings"
)

const uciPath = "/etc/config/airprint"

var defaults = map[string]string{
	"enabled":            "1",
	"listen_port":        "6310",
	"cups_host":          "127.0.0.1",
	"cups_port":          "631",
	"queue_name":         "EPSON_L3250",
	"printer_uri":        "ipps://192.168.31.6:631/ipp/print",
	"service_name":       "EPSON L3250 Series",
	"service_uuid":       "0ea66e10-844a-3a74-4c05-5b17c7daffa8",
	"dpi":                "300",
	"note":               "",
	"pdl":                "image/urf,application/pdf,image/jpeg,image/png",
	"airprint_features":  "subscription-object,airprint-1.8,airprint-1.7,airprint-1.6,airprint-1.5,airprint-1.4,airprint-1.3",
	"debug":              "0",
	"auto_recover":       "1",
	"offline_guard":      "1",
	"notify_url":         "",
	"notify_kind":        "auto",
	"notify_events":      "start,complete,error,recover",
	"history_file":       "/var/log/airprint/jobs.jsonl",
	"history_max":        "200",
}

type Target struct {
	Scheme string
	Host   string
	Port   int
	Path   string
	Secure bool
	URI    string
}

type Settings struct {
	ListenPort     int
	CupsHost       string
	CupsPort       int
	Queue          string
	Printer        Target
	ServiceName    string
	UUID           string
	DPI            int
	Note           string
	PDL            string
	URFValues      []string
	AirprintValues []string
	Debug          bool
	AutoRecover    bool
	OfflineGuard   bool
	NotifyURL      string
	NotifyKind     string
	NotifyEvents   map[string]bool
	HistoryFile    string
	HistoryMax     int
}

func loadConfig(path string) map[string]string {
	cfg := map[string]string{}
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg
	}
	inSection := false
	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "config ") {
			parts := strings.Fields(line)
			inSection = len(parts) >= 3 && parts[1] == "airprint" &&
				strings.Trim(parts[2], "'\"") == "main"
			continue
		}
		if !inSection || !strings.HasPrefix(line, "option ") {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) < 3 {
			parts = strings.SplitN(line, "\t", 3)
		}
		if len(parts) < 3 {
			continue
		}
		value := strings.TrimSpace(parts[2])
		if len(value) >= 2 && value[0] == value[len(value)-1] && (value[0] == '\'' || value[0] == '"') {
			value = value[1 : len(value)-1]
		}
		cfg[parts[1]] = value
	}
	return cfg
}

func trueVal(value string, def bool) bool {
	s := strings.ToLower(strings.TrimSpace(value))
	if s == "0" || s == "false" || s == "no" || s == "off" || s == "" {
		return false
	}
	return true
}

func parseTarget(uri string) Target {
	if uri == "" {
		uri = "ipps://127.0.0.1:631/ipp/print"
	}
	if !strings.Contains(uri, "://") {
		uri = "ipps://" + uri
	}
	u, err := url.Parse(uri)
	scheme := "ipps"
	host := "127.0.0.1"
	port := 631
	path := "/ipp/print"
	if err == nil {
		if u.Scheme != "" {
			scheme = strings.ToLower(u.Scheme)
		}
		if u.Hostname() != "" {
			host = u.Hostname()
		}
		if u.Port() != "" {
			if p, e := strconv.Atoi(u.Port()); e == nil {
				port = p
			}
		}
		if u.Path != "" {
			path = u.Path
		}
	}
	return Target{
		Scheme: scheme, Host: host, Port: port, Path: path,
		Secure: scheme == "ipps",
		URI:    scheme + "://" + host + ":" + strconv.Itoa(port) + path,
	}
}

func loadSettings(path string) Settings {
	raw := map[string]string{}
	for k, v := range defaults {
		raw[k] = v
	}
	for k, v := range loadConfig(path) {
		raw[k] = v
	}
	dpi := 300
	if strings.TrimSpace(raw["dpi"]) == "600" {
		dpi = 600
	}
	features := []string{}
	for _, x := range strings.Split(raw["airprint_features"], ",") {
		if x = strings.TrimSpace(x); x != "" {
			features = append(features, x)
		}
	}
	if len(features) == 0 {
		features = strings.Split(defaults["airprint_features"], ",")
	}
	events := map[string]bool{}
	for _, x := range strings.Split(raw["notify_events"], ",") {
		if x = strings.TrimSpace(x); x != "" {
			events[x] = true
		}
	}
	note := raw["note"]
	if note == "" {
		note = "AirPrint " + strconv.Itoa(dpi) + "dpi via CUPS capability proxy"
	}
	listen, err := strconv.Atoi(raw["listen_port"])
	if err != nil || listen <= 0 {
		listen = 6310
	}
	cupsPort, err := strconv.Atoi(raw["cups_port"])
	if err != nil || cupsPort <= 0 {
		cupsPort = 631
	}
	hmax, err := strconv.Atoi(raw["history_max"])
	if err != nil || hmax <= 0 {
		hmax = 200
	}
	if hmax < 10 {
		hmax = 10
	}
	queue := raw["queue_name"]
	if queue == "" {
		queue = "EPSON_L3250"
	}
	return Settings{
		ListenPort:     listen,
		CupsHost:       firstNonEmpty(raw["cups_host"], "127.0.0.1"),
		CupsPort:       cupsPort,
		Queue:          queue,
		Printer:        parseTarget(raw["printer_uri"]),
		ServiceName:    firstNonEmpty(raw["service_name"], "EPSON L3250 Series"),
		UUID:           firstNonEmpty(raw["service_uuid"], defaults["service_uuid"]),
		DPI:            dpi,
		Note:           note,
		PDL:            firstNonEmpty(raw["pdl"], defaults["pdl"]),
		URFValues:      []string{"V1.4", "RS" + strconv.Itoa(dpi), "DM1", "SRGB24", "W8"},
		AirprintValues: features,
		Debug:          trueVal(raw["debug"], false),
		AutoRecover:    trueVal(raw["auto_recover"], true),
		OfflineGuard:   trueVal(raw["offline_guard"], true),
		NotifyURL:      strings.TrimSpace(raw["notify_url"]),
		NotifyKind:     strings.ToLower(firstNonEmpty(strings.TrimSpace(raw["notify_kind"]), "auto")),
		NotifyEvents:   events,
		HistoryFile:    firstNonEmpty(raw["history_file"], defaults["history_file"]),
		HistoryMax:     hmax,
	}
}

func firstNonEmpty(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
