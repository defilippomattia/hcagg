package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	logger.SetOutput(io.Discard) // keep test output readable
	os.Exit(m.Run())
}

// service starts a fake service that answers every request with status,
// after delay.
func service(t *testing.T, status int, delay time.Duration) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(s.Close)
	return s.URL
}

// writeConfig writes cfg as JSON to a temporary file and returns its path.
func writeConfig(t *testing.T, cfg string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// use loads cfg as the current config, as main does at startup.
func use(t *testing.T, cfg string) {
	t.Helper()
	st, err := load(writeConfig(t, cfg))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	current.Store(st)
}

// get calls hcagg's handler like a load balancer would.
func get(path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	handle(rec, httptest.NewRequest("GET", path, nil))
	return rec
}

func check(name, url string, expected int) string {
	return `{"name":"` + name + `","url":"` + url + `","method":"GET","expected_status":` + strconv.Itoa(expected) + `}`
}

func config(extra string, checks ...string) string {
	if extra != "" {
		extra += ","
	}
	return `{` + extra + `"checks":[` + strings.Join(checks, ",") + `]}`
}

func TestHandle(t *testing.T) {
	ok := service(t, 200, 0)
	noContent := service(t, 204, 0)
	broken := service(t, 500, 0)
	slow := service(t, 200, 2*time.Second)

	tests := []struct {
		name     string
		cfg      string
		wantCode int
		wantErr  map[string]string // check name -> part of its error
	}{
		{
			name:     "all checks pass",
			cfg:      config("", check("a", ok, 200), check("b", noContent, 204)),
			wantCode: 200,
		},
		{
			name:     "wrong status fails",
			cfg:      config("", check("a", ok, 200), check("b", broken, 200)),
			wantCode: 503,
			wantErr:  map[string]string{"b": "unexpected status 500, expected 200"},
		},
		{
			name:     "status other than expected fails even if 200",
			cfg:      config("", check("a", ok, 204)),
			wantCode: 503,
			wantErr:  map[string]string{"a": "unexpected status 200, expected 204"},
		},
		{
			name:     "timeout fails",
			cfg:      config(`"timeout":"200ms"`, check("a", ok, 200), check("slow", slow, 200)),
			wantCode: 503,
			wantErr:  map[string]string{"slow": "context deadline exceeded"},
		},
		{
			name:     "connection refused fails",
			cfg:      config("", check("down", "http://127.0.0.1:1", 200)),
			wantCode: 503,
			wantErr:  map[string]string{"down": "connection refused"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			use(t, tt.cfg)
			rec := get("/health")
			if rec.Code != tt.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantCode)
			}
			var resp Response
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("body is not JSON: %v\n%s", err, rec.Body)
			}
			if resp.Healthy != (tt.wantCode == 200) {
				t.Errorf("healthy = %v with status %d", resp.Healthy, rec.Code)
			}
			for _, r := range resp.Checks {
				want, shouldFail := tt.wantErr[r.Name]
				if r.Healthy == shouldFail {
					t.Errorf("check %s: healthy = %v", r.Name, r.Healthy)
				}
				if shouldFail && !strings.Contains(r.Error, want) {
					t.Errorf("check %s: error = %q, want it to contain %q", r.Name, r.Error, want)
				}
			}
		})
	}
}

func TestHandleTimeoutIsShared(t *testing.T) {
	// Three slow checks run in parallel, so the request takes about one
	// timeout, not three.
	slow := service(t, 200, 2*time.Second)
	use(t, config(`"timeout":"300ms"`, check("a", slow, 200), check("b", slow, 200), check("c", slow, 200)))
	start := time.Now()
	get("/health")
	if d := time.Since(start); d > time.Second {
		t.Errorf("request took %v, want about 300ms", d)
	}
}

func TestHandleSimpleMode(t *testing.T) {
	ok := service(t, 200, 0)
	broken := service(t, 500, 0)

	use(t, config(`"response_mode":"simple"`, check("a", ok, 200)))
	if rec := get("/health"); rec.Code != 200 || rec.Body.String() != "OK\n" {
		t.Errorf("got %d %q, want 200 \"OK\\n\"", rec.Code, rec.Body)
	}

	use(t, config(`"response_mode":"simple"`, check("a", ok, 200), check("b", broken, 200)))
	if rec := get("/health"); rec.Code != 503 || rec.Body.String() != "FAIL\n" {
		t.Errorf("got %d %q, want 503 \"FAIL\\n\"", rec.Code, rec.Body)
	}
}

func TestHandlePath(t *testing.T) {
	ok := service(t, 200, 0)
	use(t, config(`"path":"/ready"`, check("a", ok, 200)))
	if rec := get("/ready"); rec.Code != 200 {
		t.Errorf("/ready: status = %d, want 200", rec.Code)
	}
	if rec := get("/health"); rec.Code != 404 {
		t.Errorf("/health: status = %d, want 404", rec.Code)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	cfg, err := loadConfig(writeConfig(t, config("", check("a", "http://x", 200))))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":9050" || cfg.Path != "/health" || cfg.timeout != 5*time.Second ||
		cfg.ResponseMode != "full" || cfg.logLevel != LevelInfo {
		t.Errorf("wrong defaults: %+v", cfg)
	}
}

func TestLoadConfigInvalid(t *testing.T) {
	good := check("a", "http://x", 200)
	tests := map[string]string{
		"not json":         `{`,
		"no checks":        `{"checks":[]}`,
		"no method":        `{"checks":[{"name":"a","url":"http://x","expected_status":200}]}`,
		"no status":        `{"checks":[{"name":"a","url":"http://x","method":"GET"}]}`,
		"bad timeout":      config(`"timeout":"soon"`, good),
		"bad log level":    config(`"log_level":"debug"`, good),
		"bad mode":         config(`"response_mode":"verbose"`, good),
		"cert without key": config(`"tls_cert":"c.pem"`, good),
	}
	for name, cfg := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := loadConfig(writeConfig(t, cfg)); err == nil {
				t.Error("want an error, got none")
			}
		})
	}
}

func TestLoadMissingCAFile(t *testing.T) {
	if _, err := load(writeConfig(t, config(`"ca_file":"/nonexistent.pem"`, check("a", "http://x", 200)))); err == nil {
		t.Error("want an error, got none")
	}
}

func TestReload(t *testing.T) {
	ok := service(t, 200, 0)
	broken := service(t, 500, 0)
	path := writeConfig(t, config(`"listen":":9050"`, check("a", ok, 200)))
	st, err := load(path)
	if err != nil {
		t.Fatal(err)
	}
	current.Store(st)

	// A valid change is applied.
	os.WriteFile(path, []byte(config(`"listen":":9050"`, check("a", ok, 200), check("b", broken, 200))), 0o644)
	reload(path)
	if rec := get("/health"); rec.Code != 503 {
		t.Errorf("after reload: status = %d, want 503", rec.Code)
	}

	// An invalid config is ignored, the previous one stays.
	os.WriteFile(path, []byte(`{`), 0o644)
	reload(path)
	if n := len(current.Load().cfg.Checks); n != 2 {
		t.Errorf("after invalid reload: %d checks, want 2", n)
	}

	// listen can't change on reload.
	os.WriteFile(path, []byte(config(`"listen":":9999"`, check("a", ok, 200))), 0o644)
	reload(path)
	if l := current.Load().cfg.Listen; l != ":9050" {
		t.Errorf("listen = %s, want :9050", l)
	}
}
