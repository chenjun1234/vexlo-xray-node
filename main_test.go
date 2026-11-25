package main

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLoadSettingsRejectsMissingAndMalformed(t *testing.T) {
	t.Setenv("CLIENT_ID", "")
	t.Setenv("WS_PATH", "")
	t.Setenv("PORT", "")
	if _, err := loadSettings(); err == nil || err.Error() != "CLIENT_ID is missing" {
		t.Fatalf("missing id: %v", err)
	}

	t.Setenv("CLIENT_ID", "not-a-uuid")
	t.Setenv("WS_PATH", "/0123456789abcdef")
	if _, err := loadSettings(); err == nil || err.Error() != "CLIENT_ID is malformed" {
		t.Fatalf("bad id: %v", err)
	}

	t.Setenv("CLIENT_ID", "00000000-0000-0000-0000-000000000000")
	if _, err := loadSettings(); err == nil || err.Error() != "CLIENT_ID is malformed" {
		t.Fatalf("nil id: %v", err)
	}

	t.Setenv("CLIENT_ID", "11111111-1111-4111-8111-111111111111")
	t.Setenv("WS_PATH", "")
	if _, err := loadSettings(); err == nil || err.Error() != "WS_PATH is missing" {
		t.Fatalf("missing path: %v", err)
	}
	for _, path := range []string{"/", "/short", "0123456789abcdef", "/abc/defghijklmnop", "/has space-padding", "/0123456789abcdef?ed=2560", "/................"} {
		t.Setenv("WS_PATH", path)
		if _, err := loadSettings(); err == nil || err.Error() != "WS_PATH is malformed" {
			t.Fatalf("path %q: %v", path, err)
		}
	}

	t.Setenv("WS_PATH", "/0123456789abcdef")
	for _, port := range []string{"0", "65536", "abc", "8080a", "-1"} {
		t.Setenv("PORT", port)
		if _, err := loadSettings(); err == nil || err.Error() != "PORT is malformed" {
			t.Fatalf("port %q: %v", port, err)
		}
	}
	t.Setenv("PORT", "18080")
	if _, err := loadSettings(); err == nil || err.Error() != "PORT is unsupported" {
		t.Fatalf("reserved port: %v", err)
	}
}

func TestLoadSettingsAcceptsRuntimeValues(t *testing.T) {
	id := randomUUID(t)
	path := randomPath(t)
	t.Setenv("CLIENT_ID", strings.ToUpper(id))
	t.Setenv("WS_PATH", path)
	t.Setenv("PORT", "")
	got, err := loadSettings()
	if err != nil {
		t.Fatal(err)
	}
	if got.id != id || got.path != path || got.port != 8080 {
		t.Fatal("settings were not normalized")
	}
	t.Setenv("PORT", "9090")
	got, err = loadSettings()
	if err != nil || got.port != 9090 {
		t.Fatalf("port override: %+v %v", got, err)
	}
}

func TestBuildConfigIsOneUserWithoutTLS(t *testing.T) {
	s := settings{id: randomUUID(t), path: randomPath(t), port: 8080}
	cfg := buildConfig(s)
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Log.LogLevel != "none" || cfg.Log.Access != "none" || cfg.Log.Error != "none" || cfg.Log.DNSLog {
		t.Fatal("logging is not disabled")
	}
	if len(cfg.Inbounds) != 1 || len(cfg.Outbounds) != 1 {
		t.Fatal("unexpected inbound or outbound count")
	}
	in := cfg.Inbounds[0]
	if in.Listen != "127.0.0.1" || in.Port != internalPort || in.Protocol != "vless" {
		t.Fatal("inbound listener mismatch")
	}
	if in.Settings.Decryption != "none" || len(in.Settings.Clients) != 1 || in.Settings.Clients[0].ID != s.id {
		t.Fatal("client settings mismatch")
	}
	if in.StreamSettings.Network != "ws" || in.StreamSettings.Security != "none" || in.StreamSettings.WSSettings.Path != s.path {
		t.Fatal("stream settings mismatch")
	}
	if cfg.Outbounds[0].Protocol != "freedom" {
		t.Fatal("outbound mismatch")
	}
	if bytes.Contains(raw, []byte(`"flow"`)) || bytes.Contains(raw, []byte("finalRules")) {
		t.Fatal("config gained extra features")
	}
	path, err := writeConfig(s)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %o", info.Mode().Perm())
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(body, []byte(s.id)) || !bytes.Contains(body, []byte(s.path)) {
		t.Fatal("config file missing required fields")
	}
}

func TestStripSecrets(t *testing.T) {
	got := stripSecrets([]string{"PATH=/usr/bin", "CLIENT_ID=secret", "WS_PATH=/secret", "HOME=/tmp"})
	if strings.Join(got, ",") != "PATH=/usr/bin,HOME=/tmp" {
		t.Fatalf("stripped env = %v", got)
	}
}

func TestClassify(t *testing.T) {
	const ws = "/0123456789abcdef"
	cases := []struct {
		method, path, upgrade, connection, want string
	}{
		{"GET", "/", "", "", "health"},
		{"HEAD", "/healthz", "", "", "health"},
		{"GET", "/healthz", "websocket", "upgrade", "reject"},
		{"GET", ws, "websocket", "keep-alive, Upgrade", "proxy"},
		{"GET", ws, "", "", "reject"},
		{"POST", "/", "", "", "reject"},
		{"GET", "/other", "", "", "reject"},
		{"GET", ws + "x", "websocket", "upgrade", "reject"},
	}
	for _, tc := range cases {
		if got := classify(tc.method, tc.path, tc.upgrade, tc.connection, ws); got != tc.want {
			t.Fatalf("classify %#v = %s", tc, got)
		}
	}
}

func TestHandleConnHealthProxyAndReject(t *testing.T) {
	upstream := startUpstream(t)
	wsPath := randomPath(t)
	public := startFront(t, wsPath, upstream.Addr().String())

	status, body := httpExchange(t, public, "GET / HTTP/1.1\r\nHost: t\r\n\r\n")
	if status != 200 || body != "ok\n" {
		t.Fatalf("GET / = %d %q", status, body)
	}
	status, body = httpExchange(t, public, "HEAD /healthz HTTP/1.1\r\nHost: t\r\n\r\n")
	if status != 200 || body != "" {
		t.Fatalf("HEAD /healthz = %d %q", status, body)
	}
	status, _ = httpExchange(t, public, "GET "+wsPath+" HTTP/1.1\r\nHost: t\r\n\r\n")
	if status != 404 {
		t.Fatalf("plain GET of websocket path = %d", status)
	}
	status, _ = httpExchange(t, public, "GET /nope HTTP/1.1\r\nHost: t\r\n\r\n")
	if status != 404 {
		t.Fatalf("GET /nope = %d", status)
	}
	status, _ = httpExchange(t, public, "GET http://example.com/ HTTP/1.1\r\nHost: t\r\n\r\n")
	if status != 404 {
		t.Fatalf("absolute-form = %d", status)
	}
	status, _ = httpExchange(t, public, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n")
	if status != 404 {
		t.Fatalf("CONNECT = %d", status)
	}

	client, err := net.Dial("tcp", public)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	req := "GET " + wsPath + "?ed=2560 HTTP/1.1\r\nHost: t\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n"
	if _, err := io.WriteString(client, req+"ABC"); err != nil {
		t.Fatal(err)
	}
	_ = client.SetDeadline(time.Now().Add(2 * time.Second))
	got, err := io.ReadAll(client)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "XYZ" {
		t.Fatalf("tunnel bytes = %q", got)
	}
	recorded := upstream.recorded()
	if !bytes.Contains(recorded, []byte(req)) || !bytes.Contains(recorded, []byte("ABC")) {
		t.Fatal("upstream did not receive the original upgrade bytes")
	}
	if bytes.Contains(recorded, []byte("GET / HTTP")) {
		t.Fatal("health request was forwarded")
	}
}

func TestHealthUnavailableWhenUpstreamDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	public := startFront(t, randomPath(t), addr)
	status, body := httpExchange(t, public, "GET / HTTP/1.1\r\nHost: t\r\n\r\n")
	if status != 503 || body != "" {
		t.Fatalf("down health = %d %q", status, body)
	}
}

type recordingUpstream struct {
	ln   net.Listener
	buf  chan []byte
	stop chan struct{}
}

func startUpstream(t *testing.T) *recordingUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u := &recordingUpstream{ln: ln, buf: make(chan []byte, 1), stop: make(chan struct{})}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
				reader := bufio.NewReader(conn)
				raw, err := readHeader(reader)
				if err != nil || !bytes.Contains(raw, []byte("Upgrade")) {
					return
				}
				extra := make([]byte, 32)
				n, _ := reader.Read(extra)
				u.buf <- append(append([]byte{}, raw...), extra[:n]...)
				_, _ = io.WriteString(conn, "XYZ")
			}(conn)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return u
}

func (u *recordingUpstream) Addr() net.Addr { return u.ln.Addr() }

func (u *recordingUpstream) recorded() []byte {
	select {
	case b := <-u.buf:
		return b
	case <-time.After(2 * time.Second):
		return nil
	}
}

func startFront(t *testing.T, wsPath, upstream string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				handleConn(conn, wsPath, upstream)
				conn.Close()
			}(conn)
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		<-done
	})
	return ln.Addr().String()
}

func httpExchange(t *testing.T, addr, request string) (int, string) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(conn, request); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	head, body, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok {
		t.Fatalf("short response %q", raw)
	}
	line, _, _ := bytes.Cut(head, []byte("\r\n"))
	fields := strings.Split(string(line), " ")
	if len(fields) < 2 {
		t.Fatalf("bad status line %q", line)
	}
	code := 0
	for _, c := range fields[1] {
		code = code*10 + int(c-'0')
	}
	return code, string(body)
}

func randomUUID(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[0:4]) + "-" + hex.EncodeToString(b[4:6]) + "-" + hex.EncodeToString(b[6:8]) + "-" + hex.EncodeToString(b[8:10]) + "-" + hex.EncodeToString(b[10:16])
}

func randomPath(t *testing.T) string {
	t.Helper()
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatal(err)
	}
	return "/" + hex.EncodeToString(b[:])
}
