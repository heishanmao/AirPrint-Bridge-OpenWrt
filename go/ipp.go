package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	opPrintJob = 0x0002
	opValidate = 0x0004
	opCreate   = 0x0005
	opGJA      = 0x0009
	opGetJobs  = 0x000A
	opGPA      = 0x000B
)

type ippValue struct {
	tag byte
	raw []byte
}

type ippGroup struct {
	tag   byte
	attrs map[string][]ippValue
}

func encAttr(tag byte, name string, value []byte) []byte {
	out := make([]byte, 0, 5+len(name)+len(value))
	out = append(out, tag, byte(len(name)>>8), byte(len(name)))
	out = append(out, name...)
	out = append(out, byte(len(value)>>8), byte(len(value)))
	out = append(out, value...)
	return out
}

func attrStr(tag byte, name, value string) []byte {
	return encAttr(tag, name, []byte(value))
}

func attrMulti(tag byte, name string, values []string) []byte {
	out := []byte{tag, byte(len(name) >> 8), byte(len(name))}
	out = append(out, name...)
	for i, v := range values {
		if i > 0 {
			out = append(out, tag, 0, 0)
		}
		out = append(out, byte(len(v)>>8), byte(len(v)))
		out = append(out, v...)
	}
	return out
}

func attrRange(name string, low, high int32) []byte {
	v := make([]byte, 8)
	binary.BigEndian.PutUint32(v[0:4], uint32(low))
	binary.BigEndian.PutUint32(v[4:8], uint32(high))
	return encAttr(0x33, name, v)
}

func attrInt(name string, value int32) []byte {
	v := make([]byte, 4)
	binary.BigEndian.PutUint32(v, uint32(value))
	return encAttr(0x21, name, v)
}

func parseGroups(data []byte) []ippGroup {
	groups := []ippGroup{}
	if len(data) < 8 {
		return groups
	}
	pos := 8
	var cur *ippGroup
	curName := ""
	for pos < len(data) {
		tag := data[pos]
		pos++
		if tag == 0x03 {
			break
		}
		if tag < 0x10 {
			groups = append(groups, ippGroup{tag: tag, attrs: map[string][]ippValue{}})
			cur = &groups[len(groups)-1]
			curName = ""
			continue
		}
		if cur == nil {
			groups = append(groups, ippGroup{tag: 0x01, attrs: map[string][]ippValue{}})
			cur = &groups[len(groups)-1]
		}
		if pos+2 > len(data) {
			break
		}
		nlen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if pos+nlen > len(data) {
			break
		}
		name := string(data[pos : pos+nlen])
		pos += nlen
		if name != "" {
			curName = name
		}
		if pos+2 > len(data) {
			break
		}
		vlen := int(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if pos+vlen > len(data) {
			break
		}
		value := data[pos : pos+vlen]
		pos += vlen
		if curName != "" {
			cur.attrs[curName] = append(cur.attrs[curName], ippValue{tag: tag, raw: value})
		}
	}
	return groups
}

func parseAttrs(data []byte) map[string][]ippValue {
	attrs := map[string][]ippValue{}
	for _, g := range parseGroups(data) {
		for name, values := range g.attrs {
			attrs[name] = append(attrs[name], values...)
		}
	}
	return attrs
}

func attrText(attrs map[string][]ippValue, name, def string) string {
	values := attrs[name]
	if len(values) == 0 {
		return def
	}
	return string(values[0].raw)
}

func attrNumber(attrs map[string][]ippValue, name string, def int) int {
	values := attrs[name]
	if len(values) == 0 {
		return def
	}
	v := values[0]
	if (v.tag == 0x21 || v.tag == 0x23) && len(v.raw) == 4 {
		return int(int32(binary.BigEndian.Uint32(v.raw)))
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(v.raw)), "%d", &n); err == nil {
		return n
	}
	return def
}

func attrBool(attrs map[string][]ippValue, name string, def bool) bool {
	values := attrs[name]
	if len(values) == 0 {
		return def
	}
	v := values[0]
	if v.tag == 0x22 && len(v.raw) > 0 {
		return v.raw[0] != 0
	}
	switch strings.ToLower(strings.TrimSpace(string(v.raw))) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no", "":
		return false
	}
	return def
}

func attrList(attrs map[string][]ippValue, name string) []string {
	out := []string{}
	for _, v := range attrs[name] {
		out = append(out, string(v.raw))
	}
	return out
}

func ippStatus(data []byte) int {
	if len(data) < 8 {
		return -1
	}
	return int(binary.BigEndian.Uint16(data[2:4]))
}

func stripAttr(body []byte, name []byte) []byte {
	if len(body) < 8 {
		return body
	}
	out := append([]byte{}, body[:8]...)
	pos := 8
	for pos < len(body) {
		tag := body[pos]
		if tag == 0x03 {
			out = append(out, body[pos:]...)
			break
		}
		if tag < 0x10 {
			out = append(out, body[pos])
			pos++
			continue
		}
		if pos+3 > len(body) {
			out = append(out, body[pos:]...)
			break
		}
		nlen := int(binary.BigEndian.Uint16(body[pos+1 : pos+3]))
		if pos+3+nlen > len(body) {
			out = append(out, body[pos:]...)
			break
		}
		start := pos
		nm := body[pos+3 : pos+3+nlen]
		p := pos + 3 + nlen
		for p+2 <= len(body) {
			vlen := int(binary.BigEndian.Uint16(body[p : p+2]))
			if p+2+vlen > len(body) {
				break
			}
			p += 2 + vlen
			if p >= len(body) {
				break
			}
			nt := body[p]
			if nt < 0x10 || nt == 0x01 || nt == 0x02 || nt == 0x03 {
				break
			}
			if nt == tag && p+3 <= len(body) {
				n2 := int(binary.BigEndian.Uint16(body[p+1 : p+3]))
				if n2 == 0 {
					p += 3
					continue
				}
			}
			break
		}
		if !bytes.Equal(nm, name) {
			out = append(out, body[start:p]...)
		}
		pos = p
	}
	return out
}

func appendAttr(body, blob []byte) []byte {
	idx := bytes.LastIndexByte(body, 0x03)
	if idx < 0 {
		idx = len(body)
	}
	out := make([]byte, 0, len(body)+len(blob))
	out = append(out, body[:idx]...)
	out = append(out, blob...)
	out = append(out, body[idx:]...)
	return out
}

func keywordValues(body []byte, name string) []string {
	if len(body) < 8 {
		return nil
	}
	pos := 8
	key := []byte(name)
	for pos < len(body) {
		tag := body[pos]
		if tag == 0x03 {
			break
		}
		if tag < 0x10 {
			pos++
			continue
		}
		if pos+3 > len(body) {
			break
		}
		nlen := int(binary.BigEndian.Uint16(body[pos+1 : pos+3]))
		if pos+3+nlen > len(body) {
			break
		}
		nm := body[pos+3 : pos+3+nlen]
		p := pos + 3 + nlen
		values := []string{}
		for p+2 <= len(body) {
			vlen := int(binary.BigEndian.Uint16(body[p : p+2]))
			if p+2+vlen > len(body) {
				break
			}
			values = append(values, string(body[p+2:p+2+vlen]))
			p += 2 + vlen
			if p+3 <= len(body) {
				nt := body[p]
				n2 := int(binary.BigEndian.Uint16(body[p+1 : p+3]))
				if nt == tag && n2 == 0 {
					p += 3
					continue
				}
			}
			break
		}
		if bytes.Equal(nm, key) {
			return values
		}
		pos = p
	}
	return nil
}

func fixJobReason(body []byte) []byte {
	if len(body) < 8 {
		return body
	}
	state := -1
	reasons := []string{}
	pos := 8
	for pos < len(body) {
		tag := body[pos]
		if tag == 0x03 {
			break
		}
		if tag < 0x10 {
			pos++
			continue
		}
		nlen := int(binary.BigEndian.Uint16(body[pos+1 : pos+3]))
		if pos+5+nlen > len(body) {
			break
		}
		name := string(body[pos+3 : pos+3+nlen])
		vlen := int(binary.BigEndian.Uint16(body[pos+3+nlen : pos+5+nlen]))
		value := body[pos+5+nlen : pos+5+nlen+vlen]
		pos += 5 + nlen + vlen
		if name == "job-state" && (tag == 0x21 || tag == 0x23) && len(value) == 4 {
			state = int(int32(binary.BigEndian.Uint32(value)))
		} else if name == "job-state-reasons" {
			reasons = append(reasons, string(value))
		}
	}
	if state < 0 {
		return body
	}
	var reason string
	switch state {
	case 9:
		reason = "job-completed-successfully"
		for _, r := range reasons {
			if strings.Contains(r, "error") {
				reason = "job-completed-with-errors"
				break
			}
		}
	case 8:
		reason = "aborted-by-system"
	case 7:
		reason = "job-canceled-by-user"
	case 6:
		reason = "job-stopped"
	default:
		return body
	}
	if len(reasons) == 1 && reasons[0] == reason {
		return body
	}
	return appendAttr(stripAttr(body, []byte("job-state-reasons")), attrMulti(0x44, "job-state-reasons", []string{reason}))
}

func enhanceGPA(body []byte, urfValues, airprintValues []string) []byte {
	for _, name := range [][]byte{[]byte("urf-supported"), []byte("ipp-features-supported"),
		[]byte("job-impressions-supported"), []byte("job-media-sheets-supported"),
		[]byte("printer-mandatory-job-attributes")} {
		body = stripAttr(body, name)
	}
	body = appendAttr(body, attrMulti(0x44, "urf-supported", urfValues))
	body = appendAttr(body, attrMulti(0x44, "ipp-features-supported", airprintValues))
	body = appendAttr(body, attrRange("job-impressions-supported", 1, 99999))
	body = appendAttr(body, attrRange("job-media-sheets-supported", 1, 99999))
	body = appendAttr(body, attrMulti(0x44, "printer-mandatory-job-attributes",
		[]string{"job-impressions", "job-media-sheets"}))
	creation := keywordValues(body, "job-creation-attributes-supported")
	if len(creation) > 0 {
		for _, v := range []string{"job-impressions", "job-media-sheets"} {
			found := false
			for _, c := range creation {
				if c == v {
					found = true
					break
				}
			}
			if !found {
				creation = append(creation, v)
			}
		}
		body = stripAttr(body, []byte("job-creation-attributes-supported"))
		body = appendAttr(body, attrMulti(0x44, "job-creation-attributes-supported", creation))
	}
	return body
}

func rewriteURIAttrs(body []byte, target string) []byte {
	if len(body) < 8 {
		return body
	}
	repl := func(value []byte) []byte {
		for _, scheme := range [][]byte{[]byte("ipp://"), []byte("ipps://"), []byte("http://"), []byte("https://")} {
			if !bytes.HasPrefix(value, scheme) {
				continue
			}
			rest := value[len(scheme):]
			slash := bytes.IndexByte(rest, '/')
			authority := rest
			var tail []byte
			if slash >= 0 {
				authority = rest[:slash]
				tail = rest[slash:]
			}
			host := authority
			if i := bytes.IndexByte(host, ':'); i >= 0 {
				host = host[:i]
			}
			h := strings.ToLower(string(host))
			if h == "127.0.0.1" || h == "localhost" || strings.HasSuffix(h, ".local") {
				out := append([]byte{}, scheme...)
				out = append(out, target...)
				out = append(out, tail...)
				return out
			}
		}
		return value
	}
	out := append([]byte{}, body[:8]...)
	pos := 8
	for pos < len(body) {
		tag := body[pos]
		if tag == 0x03 {
			out = append(out, body[pos:]...)
			break
		}
		if tag < 0x10 {
			out = append(out, tag)
			pos++
			continue
		}
		if pos+3 > len(body) {
			out = append(out, body[pos:]...)
			break
		}
		nlen := int(binary.BigEndian.Uint16(body[pos+1 : pos+3]))
		if pos+3+nlen > len(body) {
			out = append(out, body[pos:]...)
			break
		}
		name := body[pos+3 : pos+3+nlen]
		p := pos + 3 + nlen
		values := [][]byte{}
		for {
			if p+2 > len(body) {
				break
			}
			vlen := int(binary.BigEndian.Uint16(body[p : p+2]))
			if p+2+vlen > len(body) {
				break
			}
			values = append(values, body[p+2:p+2+vlen])
			p += 2 + vlen
			if p+3 <= len(body) {
				nt := body[p]
				n2 := int(binary.BigEndian.Uint16(body[p+1 : p+3]))
				if nt == tag && n2 == 0 {
					p += 3
					continue
				}
			}
			break
		}
		out = append(out, tag, byte(nlen>>8), byte(nlen))
		out = append(out, name...)
		for i, v := range values {
			if tag == 0x45 {
				v = repl(v)
			}
			if i > 0 {
				out = append(out, tag, 0, 0)
			}
			out = append(out, byte(len(v)>>8), byte(len(v)))
			out = append(out, v...)
		}
		pos = p
	}
	return out
}

func httpIPP(host string, port int, path string, body []byte, timeout time.Duration) ([]byte, error) {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, fmt.Sprint(port)), timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	head := fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s:%d\r\nContent-Type: application/ipp\r\n"+
		"Content-Length: %d\r\nConnection: close\r\nAccept-Encoding: identity\r\n\r\n",
		path, host, port, len(body))
	if _, err := conn.Write([]byte(head)); err != nil {
		return nil, err
	}
	if _, err := conn.Write(body); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(conn)
	if err != nil {
		return nil, err
	}
	idx := bytes.Index(data, []byte("\r\n\r\n"))
	if idx < 0 {
		return nil, fmt.Errorf("bad ipp response")
	}
	return data[idx+4:], nil
}

func ippError(req []byte, message string) []byte {
	reqid := uint32(1)
	version := []byte{0x02, 0x00}
	if len(req) >= 8 {
		reqid = binary.BigEndian.Uint32(req[4:8])
		version = req[:2]
	}
	out := append([]byte{}, version...)
	out = append(out, 0x05, 0x03)
	id := make([]byte, 4)
	binary.BigEndian.PutUint32(id, reqid)
	out = append(out, id...)
	out = append(out, 0x01)
	out = append(out, attrStr(0x47, "attributes-charset", "utf-8")...)
	out = append(out, attrStr(0x48, "attributes-natural-language", "zh-cn")...)
	out = append(out, attrStr(0x41, "status-message", message)...)
	out = append(out, 0x03)
	return out
}
