package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestConfig(t *testing.T) {
	m := New(t.TempDir(), "me@example.com", "tok")
	cfg := m.Config([]Route{
		{Hosts: []string{"shop.example.com"}, Upstreams: []string{"dw-shop-production-blue-web-1:8000"}, App: "shop"},
		{Hosts: []string{"www.shop.example.com"}, RedirectTo: "shop.example.com"},
		{Hosts: []string{"wiki.example.com"}, App: "wiki"},
		{Hosts: []string{"down.example.com"}, App: "down", Stopped: true},
		{Hosts: []string{"shop.localhost"}, Upstreams: []string{"x:1"}},
	})
	b, _ := json.Marshal(cfg)
	s := string(b)
	for _, want := range []string{
		`"dial":"dw-shop-production-blue-web-1:8000"`,
		`"Location":["https://shop.example.com{http.request.uri}"]`,
		`not running yet`,
		`temporarily unavailable for maintenance`,
		`"subjects":["shop.localhost"]`,
		`"module":"internal"`,
		`"email":"me@example.com"`,
		`"body":"tok"`,
		`"listen":"unix//run/caddy/admin.sock"`,
	} {
		if !strings.Contains(s, want) {
			t.Errorf("config lacks %s", want)
		}
	}
	if HashConfig(cfg) != HashConfig(m.Config([]Route{
		{Hosts: []string{"shop.localhost"}, Upstreams: []string{"x:1"}},
		{Hosts: []string{"down.example.com"}, App: "down", Stopped: true},
		{Hosts: []string{"wiki.example.com"}, App: "wiki"},
		{Hosts: []string{"www.shop.example.com"}, RedirectTo: "shop.example.com"},
		{Hosts: []string{"shop.example.com"}, Upstreams: []string{"dw-shop-production-blue-web-1:8000"}, App: "shop"},
	})) {
		t.Error("config hash depends on route order")
	}
}

func TestIsLocalName(t *testing.T) {
	for h, want := range map[string]bool{"a.localhost": true, "x.test": true, "10.0.0.2": true, "example.com": false, "nas.home.arpa": true} {
		if IsLocalName(h) != want {
			t.Errorf("IsLocalName(%s) != %v", h, want)
		}
	}
}
