package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaults(t *testing.T) {
	c, err := Parse(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Port != 8767 {
		t.Errorf("port = %d", c.Port)
	}
	if c.Bind != "0.0.0.0" {
		t.Errorf("bind = %s", c.Bind)
	}
	if c.CacheMB != 200 {
		t.Errorf("cache-mb = %d", c.CacheMB)
	}
	if c.CacheTTL != 60 || c.PlaylistCacheTTL != 2 || c.SessionTTL != 7200 {
		t.Errorf("engine defaults wrong: %+v", c)
	}
	if filepath.Base(c.MetaJSON) != "proxy-cache-state-rs.json" {
		t.Errorf("meta json = %s", c.MetaJSON)
	}
	if filepath.Base(c.DeviceJSON) != "device-state-rs.json" {
		t.Errorf("device json = %s", c.DeviceJSON)
	}
}

func TestPositionalPortAlone(t *testing.T) {
	c, err := Parse([]string{"9123"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Port != 9123 {
		t.Errorf("port = %d", c.Port)
	}
}

func TestPositionalPortBeforeFlags(t *testing.T) {
	// 原 argparse 支持这种写法：ysp-live.py 9123 --only-4k
	c, err := Parse([]string{"9123", "--only-4k"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Port != 9123 || !c.Only4K {
		t.Errorf("port=%d only4k=%v", c.Port, c.Only4K)
	}
}

func TestPositionalPortAfterFlags(t *testing.T) {
	c, err := Parse([]string{"--only-4k", "9123"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.Port != 9123 || !c.Only4K {
		t.Errorf("port=%d only4k=%v", c.Port, c.Only4K)
	}
}

func TestValueFlagNotTreatedAsPositional(t *testing.T) {
	c, err := Parse([]string{"--links-per-device", "0"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.LinksPerDevice != 0 {
		t.Errorf("links = %d", c.LinksPerDevice)
	}
	if c.Port != 8767 {
		t.Errorf("port should stay default, got %d", c.Port)
	}
}

func TestExtraPositionalRejected(t *testing.T) {
	if _, err := Parse([]string{"8767", "extra"}); err == nil {
		t.Fatal("expected error for extra positional")
	}
}

func TestEnvOverrides(t *testing.T) {
	t.Setenv("YSP_LINKS_PER_DEVICE", "4")
	t.Setenv("YSP_ZAP_DEBOUNCE", "0")
	c, err := Parse(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if c.LinksPerDevice != 4 {
		t.Errorf("links = %d", c.LinksPerDevice)
	}
	if c.ZapDebounce != 0 {
		t.Errorf("zap = %v", c.ZapDebounce)
	}
}

func TestOnly4KEnv(t *testing.T) {
	t.Setenv("YSP_ONLY_4K_DEVICE", "1")
	c, err := Parse(nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !c.Only4K {
		t.Error("YSP_ONLY_4K_DEVICE=1 should enable Only4K")
	}
}

func TestDataDirCreated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", "data")
	c, err := Parse([]string{"--data-dir", dir})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("data dir not created: %v", err)
	}
	if c.MetaJSON != filepath.Join(dir, "proxy-cache-state-rs.json") {
		t.Errorf("meta json = %s", c.MetaJSON)
	}
}
