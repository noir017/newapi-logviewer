package main

import (
	"net/http/httptest"
	"os"
	"testing"
)

// BASE_PATH normalization decides where every route lives, so a regression here
// breaks the whole deployment. "/" must collapse to "" (serve at the root) and
// stray slashes must not produce "//logviewer" or "/logviewer/".
func TestBasePathNormalization(t *testing.T) {
	cases := map[string]string{
		"":            "/logviewer", // default
		"/logviewer":  "/logviewer",
		"logviewer":   "/logviewer",
		"/logviewer/": "/logviewer",
		"//logviewer": "/logviewer",
		"/":           "",
		"///":         "",
		"/a/b":        "/a/b",
	}
	for in, want := range cases {
		if in == "" {
			os.Unsetenv("BASE_PATH")
		} else {
			os.Setenv("BASE_PATH", in)
		}
		if got := loadConfig().Base; got != want {
			t.Errorf("BASE_PATH=%q -> %q, want %q", in, got, want)
		}
	}
	os.Unsetenv("BASE_PATH")
}

// Serving at the root is a supported configuration and takes a different code
// path than a prefixed mount.
func TestRootMount(t *testing.T) {
	cfg := loadConfig()
	cfg.LogDir = "./testdata"
	cfg.AuthMode = "none"
	cfg.Base = ""
	srv := newServer(cfg)

	for _, p := range []string{"/", "/healthz", "/api/calls"} {
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
		if w.Code != 200 {
			t.Errorf("%s -> %d, want 200", p, w.Code)
		}
	}
}

func TestEnvHelpers(t *testing.T) {
	os.Setenv("LV_T", "true")
	if !envBool("LV_T", false) {
		t.Error(`envBool("true") = false`)
	}
	os.Setenv("LV_T", "0")
	if envBool("LV_T", true) {
		t.Error(`envBool("0") = true`)
	}
	os.Unsetenv("LV_T")
	if !envBool("LV_T", true) {
		t.Error("unset should fall back to the default")
	}
	// A malformed value must fall back rather than silently becoming 0, which
	// for LIMIT_MB would mean "parse the whole file".
	os.Setenv("LV_N", "not-a-number")
	if got := envInt64("LV_N", 40); got != 40 {
		t.Errorf("envInt64(garbage) = %d, want the default 40", got)
	}
	os.Unsetenv("LV_N")
}
