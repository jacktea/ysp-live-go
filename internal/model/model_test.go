package model

import (
	"os"
	"path/filepath"
	"testing"
)

func TestChannelDirectory(t *testing.T) {
	d := NewChannelDirectory()
	if d.Count() != 64 {
		t.Fatalf("count = %d, want 64", d.Count())
	}
	cctv, weishi := 0, 0
	for _, m := range d.AllChannels() {
		switch m.Group {
		case "央视":
			cctv++
		case "卫视":
			weishi++
		}
	}
	if cctv != 30 || weishi != 34 {
		t.Fatalf("cctv=%d weishi=%d, want 30/34", cctv, weishi)
	}
}

func TestGetMetaAlias(t *testing.T) {
	d := NewChannelDirectory()
	// 别名 anhuiws -> ahws
	m := d.GetMeta("anhuiws")
	if m == nil || m.Slug != "ahws" {
		t.Fatalf("alias lookup failed: %+v", m)
	}
	// /live/xxx.m3u8 形式
	m = d.GetMeta("CCTV16-4K.m3u8")
	if m == nil || m.Slug != "cctv164k" {
		t.Fatalf("normalize failed: %+v", m)
	}
	if d.GetMeta("nosuchchannel") != nil {
		t.Fatal("expected nil for unknown channel")
	}
}

func TestComputeXUIDStable(t *testing.T) {
	p := DeviceProfile{
		AndroidID: "0123456789abcdef", MAC: "02:00:00:00:00:01",
		Hardware: "mt5895", Board: "mt5895", Brand: "Sony", Device: "sony_xr85z9k",
		Manufacturer: "Sony", Model: "XR-85Z9K", Product: "sony_xr85z9k",
		Tags: "release-keys", BuildType: "user", User: "build", Resolution: "7680*4320",
	}
	a := ComputeXUID(p)
	b := ComputeXUID(p)
	if a != b {
		t.Fatal("ComputeXUID must be deterministic")
	}
	if len(a) != 40 {
		t.Fatalf("x_uid length = %d, want 40 (sha1 hex)", len(a))
	}
}

func TestDeviceStatePersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "device-state-rs.json")
	store := NewDeviceStateStore(path)

	if _, existed := store.Load(); existed {
		t.Fatal("should not exist initially")
	}
	st := DefaultDeviceState()
	if err := store.Save(st); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, existed := store.Load()
	if !existed {
		t.Fatal("should exist after save")
	}
	if loaded.Profile.AndroidID != st.Profile.AndroidID || loaded.Profile.MAC != st.Profile.MAC {
		t.Fatalf("roundtrip mismatch: %+v vs %+v", loaded.Profile, st.Profile)
	}
	if len(loaded.Profile.AndroidID) != 16 {
		t.Fatalf("android_id length = %d", len(loaded.Profile.AndroidID))
	}
}

func TestDeviceStateCorruptFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := NewDeviceStateStore(path)
	if _, existed := store.Load(); existed {
		t.Fatal("corrupt file should report not existed")
	}
}

func TestVDNAppCommonOrder(t *testing.T) {
	got := VDNAppCommon("1.4.1")
	want := `{"adid":"","an":"央视频电视投屏助手","ap":"cctv_app_tv","av":"1.4.1"}`
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestInferOSVersionAndSDK(t *testing.T) {
	p := DeviceProfile{Fingerprint: "Sony/sony_xr85z9k/sony_xr85z9k:13/SONYTV.2022.XR_85Z9K/2024:user/release-keys"}
	if got := InferOSVersion(p); got != "13" {
		t.Fatalf("os version = %q", got)
	}
	if got := InferSDKInt(p); got != "33" {
		t.Fatalf("sdk = %q", got)
	}
}
