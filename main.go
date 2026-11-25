// Command entrypoint is the container process for one authenticated VLESS
// WebSocket user. It writes a runtime config, starts Xray on loopback, and
// accepts public connections on 0.0.0.0:$PORT.
//
// Xray's WebSocket handler answers every other HTTP path with 404, including
// probes of "/". This process answers GET and HEAD / and /healthz itself, and
// forwards only an HTTP/1.1 WebSocket upgrade whose path matches WS_PATH.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	xrayBin      = "/usr/local/bin/xray"
	internalPort = 18080
	internalAddr = "127.0.0.1:18080"
	maxHeader    = 8192
	maxTunnels   = 256
)

var (
	uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[1-8][0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`)
	pathRe = regexp.MustCompile(`^/[A-Za-z0-9._~-]{16,128}$`)
	portRe = regexp.MustCompile(`^[0-9]{1,5}$`)
)

type settings struct {
	id   string
	path string
	port int
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := runHealthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy")
			os.Exit(1)
		}
		os.Exit(0)
	}
	if len(os.Args) != 1 {
		fmt.Fprintln(os.Stderr, "startup refused: unknown command")
		os.Exit(1)
	}
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "startup refused: %s\n", err.Error())
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadSettings()
	if err != nil {
		return err
	}
	path, err := writeConfig(cfg)
	if err != nil {
		return errors.New("config write failed")
	}
	defer os.Remove(path)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cmd := exec.Command(xrayBin, "run", "-config", path)
	cmd.Env = stripSecrets(os.Environ())
	cmd.Stdin = nil
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
	if err := cmd.Start(); err != nil {
		return errors.New("proxy process failed")
	}

	exited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(exited)
	}()
	defer stopProcess(cmd, exited)

	if err := waitReady(ctx, internalAddr); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}
	_ = os.Remove(path)

	ln, err := net.Listen("tcp4", net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.port)))
	if err != nil {
		return errors.New("listener failed")
	}
	go func() {
		select {
		case <-ctx.Done():
		case <-exited:
		}
		_ = ln.Close()
	}()

	serveErr := serve(ln, cfg.path)
	if ctx.Err() != nil {
		return nil
	}
	select {
	case <-exited:
		return errors.New("proxy process exited")
	default:
	}
	if serveErr != nil && !errors.Is(serveErr, net.ErrClosed) {
		return errors.New("listener failed")
	}
	return errors.New("proxy process exited")
}

func stopProcess(cmd *exec.Cmd, exited <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	_ = cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-exited:
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-exited
	}
}

func waitReady(ctx context.Context, addr string) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("proxy listener not ready")
		}
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func loadSettings() (settings, error) {
	id := os.Getenv("CLIENT_ID")
	if id == "" {
		return settings{}, errors.New("CLIENT_ID is missing")
	}
	if !uuidRe.MatchString(id) {
		return settings{}, errors.New("CLIENT_ID is malformed")
	}
	wsPath := os.Getenv("WS_PATH")
	if wsPath == "" {
		return settings{}, errors.New("WS_PATH is missing")
	}
	if !pathRe.MatchString(wsPath) || strings.Contains(wsPath, "..") {
		return settings{}, errors.New("WS_PATH is malformed")
	}
	portValue := os.Getenv("PORT")
	if portValue == "" {
		portValue = "8080"
	}
	port, err := parsePort(portValue)
	if err != nil {
		return settings{}, err
	}
	return settings{id: strings.ToLower(id), path: wsPath, port: port}, nil
}

func parsePort(value string) (int, error) {
	if !portRe.MatchString(value) {
		return 0, errors.New("PORT is malformed")
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return 0, errors.New("PORT is malformed")
	}
	if port == internalPort {
		return 0, errors.New("PORT is unsupported")
	}
	return port, nil
}

func stripSecrets(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		key, _, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		if key == "CLIENT_ID" || key == "WS_PATH" {
			continue
		}
		out = append(out, kv)
	}
	return out
}

type xrayConfig struct {
	Log       xrayLog        `json:"log"`
	Inbounds  []xrayInbound  `json:"inbounds"`
	Outbounds []xrayOutbound `json:"outbounds"`
}

type xrayLog struct {
	LogLevel string `json:"logLevel"`
	Access   string `json:"access"`
	Error    string `json:"error"`
	DNSLog   bool   `json:"dnsLog"`
}

type xrayInbound struct {
	Listen         string             `json:"listen"`
	Port           int                `json:"port"`
	Protocol       string             `json:"protocol"`
	Settings       vlessSettings      `json:"settings"`
	StreamSettings xrayStreamSettings `json:"streamSettings"`
}

type vlessSettings struct {
	Clients    []vlessClient `json:"clients"`
	Decryption string        `json:"decryption"`
}

type vlessClient struct {
	ID string `json:"id"`
}

type xrayStreamSettings struct {
	Network    string         `json:"network"`
	Security   string         `json:"security"`
	WSSettings xrayWSSettings `json:"wsSettings"`
}

type xrayWSSettings struct {
	Path string `json:"path"`
}

type xrayOutbound struct {
	Protocol string `json:"protocol"`
	Tag      string `json:"tag"`
}

func buildConfig(s settings) xrayConfig {
	return xrayConfig{
		Log: xrayLog{LogLevel: "none", Access: "none", Error: "none", DNSLog: false},
		Inbounds: []xrayInbound{{
			Listen:   "127.0.0.1",
			Port:     internalPort,
			Protocol: "vless",
			Settings: vlessSettings{
				Clients:    []vlessClient{{ID: s.id}},
				Decryption: "none",
			},
			StreamSettings: xrayStreamSettings{
				Network:    "ws",
				Security:   "none",
				WSSettings: xrayWSSettings{Path: s.path},
			},
		}},
		Outbounds: []xrayOutbound{{Protocol: "freedom", Tag: "direct"}},
	}
}

func writeConfig(s settings) (string, error) {
	f, err := os.CreateTemp("/tmp", "xray-*.json")
	if err != nil {
		return "", err
	}
	name := f.Name()
	enc := json.NewEncoder(f)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(buildConfig(s)); err != nil {
		f.Close()
		os.Remove(name)
		return "", err
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return "", err
	}
	if err := os.Chmod(name, 0o600); err != nil {
		os.Remove(name)
		return "", err
	}
	return name, nil
}

func runHealthcheck() error {
	portValue := os.Getenv("PORT")
	if portValue == "" {
		portValue = "8080"
	}
	if _, err := parsePort(portValue); err != nil {
		return err
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", portValue), 2*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"); err != nil {
		return err
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	if n == 0 && err != nil {
		return err
	}
	if !bytes.HasPrefix(buf[:n], []byte("HTTP/1.1 200 ")) {
		return errors.New("bad status")
	}
	return nil
}

func serve(ln net.Listener, wsPath string) error {
	sem := make(chan struct{}, maxTunnels)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		select {
		case sem <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		go func(conn net.Conn) {
			defer func() { <-sem }()
			handleConn(conn, wsPath, internalAddr)
			conn.Close()
		}(conn)
	}
}

func handleConn(conn net.Conn, wsPath, upstream string) {
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	reader := bufio.NewReaderSize(conn, maxHeader)
	raw, err := readHeader(reader)
	if err != nil {
		return
	}
	method, target, version, upgrade, connection, err := parseRequest(raw)
	if err != nil || (version != "HTTP/1.0" && version != "HTTP/1.1") {
		return
	}
	path, ok := originPath(target)
	if !ok {
		writeSimple(conn, 404, "")
		return
	}
	switch classify(method, path, upgrade, connection, wsPath) {
	case "health":
		if !reachable(upstream) {
			writeSimple(conn, 503, "")
			return
		}
		if method == "HEAD" {
			writeSimple(conn, 200, "")
			return
		}
		writeSimple(conn, 200, "ok\n")
	case "proxy":
		up, err := net.DialTimeout("tcp", upstream, 2*time.Second)
		if err != nil {
			writeSimple(conn, 503, "")
			return
		}
		splice(conn, reader, raw, up)
	default:
		writeSimple(conn, 404, "")
	}
}

func readHeader(r *bufio.Reader) ([]byte, error) {
	var raw []byte
	for {
		line, err := r.ReadSlice('\n')
		raw = append(raw, line...)
		if len(raw) > maxHeader {
			return nil, errors.New("headers too large")
		}
		if err != nil {
			return nil, err
		}
		if bytes.Equal(line, []byte("\r\n")) {
			return raw, nil
		}
	}
}

func parseRequest(raw []byte) (method, target, version, upgrade, connection string, err error) {
	lines := bytes.Split(raw, []byte("\r\n"))
	if len(lines) < 2 {
		return "", "", "", "", "", errors.New("short request")
	}
	parts := bytes.SplitN(lines[0], []byte(" "), 3)
	if len(parts) != 3 || len(parts[0]) == 0 || len(parts[1]) == 0 || len(parts[2]) == 0 {
		return "", "", "", "", "", errors.New("bad request line")
	}
	method = strings.ToUpper(string(parts[0]))
	target = string(parts[1])
	version = string(parts[2])
	for _, line := range lines[1:] {
		if len(line) == 0 {
			continue
		}
		name, value, ok := bytes.Cut(line, []byte(":"))
		if !ok {
			return "", "", "", "", "", errors.New("bad header")
		}
		key := strings.ToLower(strings.TrimSpace(string(name)))
		val := strings.TrimSpace(string(value))
		switch key {
		case "upgrade":
			upgrade = appendHeader(upgrade, val)
		case "connection":
			connection = appendHeader(connection, val)
		}
	}
	return method, target, version, upgrade, connection, nil
}

func appendHeader(existing, value string) string {
	if existing == "" {
		return value
	}
	return existing + ", " + value
}

func originPath(target string) (string, bool) {
	if !strings.HasPrefix(target, "/") {
		return "", false
	}
	if i := strings.IndexAny(target, "?#"); i >= 0 {
		target = target[:i]
	}
	if target == "" || strings.ContainsAny(target, " \t") {
		return "", false
	}
	return target, true
}

func classify(method, path, upgrade, connection, wsPath string) string {
	websocket := hasToken(upgrade, "websocket") && hasToken(connection, "upgrade")
	if websocket && method == "GET" && path == wsPath {
		return "proxy"
	}
	if !websocket && (method == "GET" || method == "HEAD") && (path == "/" || path == "/healthz") {
		return "health"
	}
	return "reject"
}

func hasToken(header, token string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(part), token) {
			return true
		}
	}
	return false
}

func reachable(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

func writeSimple(conn net.Conn, code int, body string) {
	reason := "Error"
	switch code {
	case 200:
		reason = "OK"
	case 404:
		reason = "Not Found"
	case 503:
		reason = "Service Unavailable"
	}
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "HTTP/1.1 %d %s\r\n", code, reason)
	buf.WriteString("Content-Type: text/plain; charset=utf-8\r\n")
	buf.WriteString("Cache-Control: no-store\r\n")
	buf.WriteString("Connection: close\r\n")
	fmt.Fprintf(&buf, "Content-Length: %d\r\n\r\n", len(body))
	buf.WriteString(body)
	_, _ = conn.Write(buf.Bytes())
}

func splice(client net.Conn, reader *bufio.Reader, raw []byte, upstream net.Conn) {
	defer upstream.Close()
	_ = client.SetDeadline(time.Time{})
	_ = upstream.SetDeadline(time.Time{})
	if _, err := upstream.Write(raw); err != nil {
		return
	}
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(upstream, reader)
		closeWrite(upstream)
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, upstream)
		closeWrite(client)
		done <- struct{}{}
	}()
	<-done
	client.Close()
	upstream.Close()
	<-done
}

func closeWrite(conn net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := conn.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = conn.Close()
}
