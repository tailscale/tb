// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package imds

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandler(t *testing.T) {
	var logged []string
	h := New(Meta{Name: "vm1", Slot: 1, Image: "img", CPUs: 4}, map[string]string{"token": "abc"})
	h.OnLog = func(l string) { logged = append(logged, l) }
	srv := httptest.NewServer(h)
	defer srv.Close()

	get := func(path string) (int, string) {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, body := get("/v0/meta"); code != 200 {
		t.Fatalf("meta: %d %s", code, body)
	} else {
		var m Meta
		json.Unmarshal([]byte(body), &m)
		if m.Name != "vm1" || m.CPUs != 4 {
			t.Errorf("meta = %+v", m)
		}
	}
	if code, body := get("/v0/user-data/token"); code != 200 || body != "abc" {
		t.Errorf("user-data/token: %d %q", code, body)
	}
	if code, _ := get("/v0/user-data/nope"); code != 404 {
		t.Errorf("user-data/nope: %d", code)
	}
	if code, body := get("/v0/user-data"); code != 200 || !strings.Contains(body, `"token": "abc"`) {
		t.Errorf("user-data: %d %q", code, body)
	}
	if code, body := get("/v0/time"); code != 200 || !strings.HasSuffix(strings.TrimSpace(body), "Z") {
		t.Errorf("time: %d %q", code, body)
	}
	if code, _ := get("/v0/ready"); code != 405 {
		t.Errorf("GET ready: %d, want 405", code)
	}

	select {
	case <-h.Ready():
		t.Fatal("ready before POST")
	default:
	}
	resp, err := http.Post(srv.URL+"/v0/ready", "", nil)
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("POST ready: %v %v", resp, err)
	}
	select {
	case <-h.Ready():
	default:
		t.Fatal("not ready after POST")
	}

	resp, err = http.Post(srv.URL+"/v0/log", "text/plain", strings.NewReader("line one\nline two\n"))
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("POST log: %v %v", resp, err)
	}
	if len(logged) != 2 || logged[1] != "line two" {
		t.Errorf("logged = %q", logged)
	}

	h.SetUserData("token", "")
	if code, _ := get("/v0/user-data/token"); code != 404 {
		t.Errorf("after removal: %d", code)
	}
	h.SetMeta(func(m *Meta) { m.BootMode = "restored" })
	if _, body := get("/v0/meta"); !strings.Contains(body, `"boot_mode": "restored"`) {
		t.Errorf("meta after SetMeta: %s", body)
	}
}
