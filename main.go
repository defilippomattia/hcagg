package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// version is set at build time with -ldflags "-X main.version=v1.2.3".
var version = "dev"

type Check struct {
	Name           string `json:"name"`
	URL            string `json:"url"`
	Method         string `json:"method"`
	ExpectedStatus int    `json:"expected_status"`
}

type Config struct {
	Listen  string  `json:"listen"`
	Path    string  `json:"path"`
	Timeout string  `json:"timeout"`
	Checks  []Check `json:"checks"`

	TLSCert string `json:"tls_cert"` // if tls_cert and tls_key are set, hcagg serves HTTPS
	TLSKey  string `json:"tls_key"`

	CAFile             string `json:"ca_file"`
	InsecureSkipVerify bool   `json:"insecure_skip_verify"`

	LogLevel     string `json:"log_level"`
	ResponseMode string `json:"response_mode"`

	// parsed from the fields above
	timeout  time.Duration
	logLevel Level
}

type Level int

const (
	LevelInfo Level = iota
	LevelError
)

var (
	levelNames = [...]string{"INFO", "ERROR"}
	levels     = map[string]Level{"info": LevelInfo, "error": LevelError}
	logger     = log.New(os.Stderr, "", 0)
	minLevel   atomic.Int32 // a Level; atomic because a reload can change it while requests are logging
)

// logf prints "<timestamp> <LEVEL> <text>" if level is at or above minLevel.
func logf(level Level, format string, args ...any) {
	if level >= Level(minLevel.Load()) {
		logger.Print(formatLine(level, format, args...))
	}
}

func formatLine(level Level, format string, args ...any) string {
	return fmt.Sprintf("%s %-5s %s", time.Now().Format("2006-01-02 15:04:05.000"), levelNames[level], fmt.Sprintf(format, args...))
}

const divider = "------------------------------------------------------------"

// requestLog collects the log lines of one request to path, so they are
// printed together as one block, starting with a divider.
type requestLog struct {
	lines []string
}

func (l *requestLog) logf(level Level, format string, args ...any) {
	if level < Level(minLevel.Load()) {
		return
	}
	l.lines = append(l.lines, formatLine(level, format, args...))
}

// flush prints the block in a single write, so blocks of requests that run
// at the same time don't get mixed. Nothing is printed if there are no lines.
func (l *requestLog) flush() {
	if len(l.lines) == 0 {
		return
	}
	logger.Print(divider + "\n" + strings.Join(l.lines, "\n"))
}

type Result struct {
	Name      string `json:"name"`
	URL       string `json:"url"`
	Healthy   bool   `json:"healthy"`
	Status    int    `json:"status,omitempty"`
	LatencyMs int64  `json:"latency_ms"`
	Error     string `json:"error,omitempty"`
}

type Response struct {
	Healthy bool      `json:"healthy"`
	Time    time.Time `json:"time"`
	Checks  []Result  `json:"checks"`
}

func loadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.Listen == "" {
		cfg.Listen = ":9050"
	}
	if cfg.Path == "" {
		cfg.Path = "/health"
	}
	if cfg.Timeout == "" {
		cfg.Timeout = "5s"
	}
	if cfg.timeout, err = time.ParseDuration(cfg.Timeout); err != nil {
		return nil, fmt.Errorf("timeout: %w", err)
	}
	if cfg.ResponseMode == "" {
		cfg.ResponseMode = "full"
	}
	cfg.ResponseMode = strings.ToLower(cfg.ResponseMode)
	if cfg.ResponseMode != "simple" && cfg.ResponseMode != "full" {
		return nil, fmt.Errorf("response_mode %q: must be simple or full", cfg.ResponseMode)
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	var ok bool
	if cfg.logLevel, ok = levels[strings.ToLower(cfg.LogLevel)]; !ok {
		return nil, fmt.Errorf("log_level %q: must be info or error", cfg.LogLevel)
	}
	if (cfg.TLSCert == "") != (cfg.TLSKey == "") {
		return nil, fmt.Errorf("tls_cert and tls_key must be set together")
	}
	if len(cfg.Checks) == 0 {
		return nil, fmt.Errorf("no checks defined")
	}
	for i := range cfg.Checks {
		c := &cfg.Checks[i]
		if c.Name == "" || c.URL == "" || c.Method == "" || c.ExpectedStatus == 0 {
			return nil, fmt.Errorf("check #%d: name, url, method and expected_status are required", i+1)
		}
		c.Method = strings.ToUpper(c.Method)
	}
	return &cfg, nil
}

func newClient(cfg *Config) (*http.Client, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, fmt.Errorf("ca_file: %w", err)
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("ca_file: no valid PEM certificates in %s", cfg.CAFile)
		}
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = &tls.Config{
		RootCAs:            pool,
		InsecureSkipVerify: cfg.InsecureSkipVerify,
	}
	return &http.Client{Transport: tr}, nil
}

func runCheck(ctx context.Context, client *http.Client, c Check) (r Result) {
	r = Result{Name: c.Name, URL: c.URL}

	start := time.Now()
	defer func() { r.LatencyMs = time.Since(start).Milliseconds() }()

	req, err := http.NewRequestWithContext(ctx, c.Method, c.URL, nil)
	if err != nil {
		r.Error = err.Error()
		return r
	}

	resp, err := client.Do(req)
	if err != nil {
		// Drop the `Get "url":` prefix, name and URL are reported separately.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		r.Error = err.Error()
		return r
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	r.Status = resp.StatusCode
	r.Healthy = resp.StatusCode == c.ExpectedStatus
	if !r.Healthy {
		r.Error = fmt.Sprintf("unexpected status %d, expected %d", resp.StatusCode, c.ExpectedStatus)
	}
	return r
}

// state is what a reload replaces. Each request uses the state that was
// current when it started.
type state struct {
	cfg    *Config
	client *http.Client
}

var current atomic.Pointer[state]

func handle(w http.ResponseWriter, req *http.Request) {
	st := current.Load()
	cfg, client := st.cfg, st.client
	if req.URL.Path != cfg.Path {
		http.NotFound(w, req)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), cfg.timeout)
	defer cancel()

	results := make([]Result, len(cfg.Checks))
	var wg sync.WaitGroup
	for i, c := range cfg.Checks {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runCheck(ctx, client, c)
		}()
	}
	wg.Wait()

	resp := Response{Healthy: true, Time: time.Now().UTC(), Checks: results}
	var rl requestLog
	var passed, failed []string
	for i, r := range results {
		c := cfg.Checks[i]
		if r.Healthy {
			passed = append(passed, r.Name)
			rl.logf(LevelInfo, "check %s passed: %s %s returned %d in %dms", r.Name, c.Method, r.URL, r.Status, r.LatencyMs)
		} else {
			resp.Healthy = false
			failed = append(failed, r.Name)
			rl.logf(LevelError, "check %s failed: %s %s: %s (%dms)", r.Name, c.Method, r.URL, r.Error, r.LatencyMs)
		}
	}

	if resp.Healthy {
		rl.logf(LevelInfo, "all %d checks passed: %s", len(results), strings.Join(passed, ", "))
	} else if len(passed) == 0 {
		rl.logf(LevelInfo, "all %d checks failed: %s", len(results), strings.Join(failed, ", "))
	} else {
		rl.logf(LevelInfo, "%d of %d checks failed: %s (passed: %s)",
			len(failed), len(results), strings.Join(failed, ", "), strings.Join(passed, ", "))
	}
	code := http.StatusOK
	if !resp.Healthy {
		code = http.StatusServiceUnavailable
	}
	rl.flush()

	w.Header().Set("Cache-Control", "no-store")
	if cfg.ResponseMode == "simple" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(code)
		if resp.Healthy {
			fmt.Fprintln(w, "OK")
		} else {
			fmt.Fprintln(w, "FAIL")
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(resp)
}

// load reads the config and builds the HTTP client for it.
func load(path string) (*state, error) {
	cfg, err := loadConfig(path)
	if err != nil {
		return nil, err
	}
	client, err := newClient(cfg)
	if err != nil {
		return nil, err
	}
	return &state{cfg, client}, nil
}

// logConfig prints the config at startup and on reload, whatever log_level is.
// It's one block: a header line, then the settings and checks indented.
func logConfig(msg string, cfg *Config) {
	scheme := "http"
	if cfg.TLSCert != "" {
		scheme = "https"
	}
	var b strings.Builder
	b.WriteString(formatLine(LevelInfo, "%s", msg))
	fmt.Fprintf(&b, "\n    listen:         %s://%s%s", scheme, cfg.Listen, cfg.Path)
	fmt.Fprintf(&b, "\n    timeout:        %s", cfg.timeout)
	fmt.Fprintf(&b, "\n    log level:      %s", strings.ToLower(cfg.LogLevel))
	fmt.Fprintf(&b, "\n    response mode:  %s", cfg.ResponseMode)
	if cfg.InsecureSkipVerify {
		b.WriteString("\n    insecure_skip_verify is on, backend TLS certificates are not verified")
	}
	nameW, methodW, urlW := 0, 0, 0
	for _, c := range cfg.Checks {
		nameW, methodW, urlW = max(nameW, len(c.Name)), max(methodW, len(c.Method)), max(urlW, len(c.URL))
	}
	fmt.Fprintf(&b, "\n    checks (%d):", len(cfg.Checks))
	for _, c := range cfg.Checks {
		fmt.Fprintf(&b, "\n      %-*s  %-*s %-*s  expects %d", nameW, c.Name, methodW, c.Method, urlW, c.URL, c.ExpectedStatus)
	}
	logger.Print(b.String())
}

// reload loads the config again. If it's invalid, the current config stays.
// listen, tls_cert and tls_key are kept from the running config, since
// changing them needs a restart.
func reload(path string) {
	old := current.Load()
	st, err := load(path)
	if err != nil {
		logf(LevelError, "reload failed, keeping the current config: %v", err)
		return
	}
	c, o := st.cfg, old.cfg
	if c.Listen != o.Listen || c.TLSCert != o.TLSCert || c.TLSKey != o.TLSKey {
		logf(LevelError, "listen, tls_cert and tls_key can't change on reload, restart hcagg to apply them")
		c.Listen, c.TLSCert, c.TLSKey = o.Listen, o.TLSCert, o.TLSKey
	}
	minLevel.Store(int32(c.logLevel))
	current.Store(st)
	old.client.CloseIdleConnections()
	logConfig("reloaded config "+path, c)
}

func fatal(msg string, err error) {
	logger.Print(formatLine(LevelError, "%s: %v", msg, err)) // always printed
	os.Exit(1)
}

func main() {
	cfgPath := flag.String("config", "config.json", "path to config file")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}

	st, err := load(*cfgPath)
	if err != nil {
		fatal("invalid config", err)
	}
	cfg := st.cfg
	minLevel.Store(int32(cfg.logLevel))
	current.Store(st)
	logConfig("starting hcagg "+version+" with config "+*cfgPath, cfg)

	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for range hup {
			reload(*cfgPath)
		}
	}()

	if cfg.TLSCert != "" {
		err = http.ListenAndServeTLS(cfg.Listen, cfg.TLSCert, cfg.TLSKey, http.HandlerFunc(handle))
	} else {
		err = http.ListenAndServe(cfg.Listen, http.HandlerFunc(handle))
	}
	fatal("server stopped", err)
}

//test release 1
