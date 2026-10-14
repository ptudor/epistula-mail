package main

import (
	"crypto/sha256"
	"net/http/httptest"
	"testing"
	"time"
)

func TestValidateHTTPTimeoutsAndPool(t *testing.T) {
	for name, mutate := range map[string]func(*Config){
		"read":             func(c *Config) { c.Server.ReadTimeoutSec = -1 },
		"write":            func(c *Config) { c.Server.WriteTimeoutSec = -1 },
		"idle":             func(c *Config) { c.Server.IdleTimeoutSec = -1 },
		"shutdown":         func(c *Config) { c.Server.ShutdownTimeoutSec = 0 },
		"admin read":       func(c *Config) { c.Admin.ReadTimeoutSec = -1 },
		"admin write":      func(c *Config) { c.Admin.WriteTimeoutSec = -1 },
		"admin shutdown":   func(c *Config) { c.Admin.ShutdownTimeoutSec = 0 },
		"pool zero":        func(c *Config) { c.Postgres.MaxOpenConns = 0 },
		"pool excess idle": func(c *Config) { c.Postgres.MaxIdleConns = c.Postgres.MaxOpenConns + 1 },
	} {
		t.Run(name, func(t *testing.T) {
			c := validBase()
			mutate(c)
			if err := c.Validate(); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}
	c := validBase()
	c.Server.ReadTimeoutSec, c.Server.WriteTimeoutSec, c.Server.IdleTimeoutSec = 0, 0, 0
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestBearerSchemeCaseInsensitive(t *testing.T) {
	a := newAuthenticator(&server{cfg: DefaultConfig()})
	a.cache[sha256.Sum256([]byte("test-token"))] = cachedToken{tok: apiToken{ID: 1}, expires: time.Now().Add(time.Minute)}
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Authorization", scheme+" test-token")
		w := httptest.NewRecorder()
		if tok := a.authenticate(w, r); tok == nil || tok.ID != 1 {
			t.Fatalf("%s: %s", scheme, w.Body.String())
		}
	}
}
