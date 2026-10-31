package gateway

import (
	"testing"
	"time"
)

func TestParseCatchupTimeVal(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		ok   bool
	}{
		{"1700000000", 1700000000, true},          // 10 位秒
		{"1700000000123", 1700000000, true},       // 13 位毫秒
		{"20240101120000", 1704081600, true},      // 14 位东八区
		{"202401011200", 1704081600, true},        // 12 位东八区
		{"2024-01-01 12:00:00", 1704081600, true}, // 含分隔符 -> 剔除非数字
		{"", 0, false},
		{"abc", 0, false},
		{"12345", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseCatchupTimeVal(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseCatchupTimeVal(%q) = (%d,%v), want (%d,%v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestParseCatchupQueryPlayseak(t *testing.T) {
	start, end, ok := ParseCatchupQuery("playseek=20240101120000-20240101130000")
	if !ok {
		t.Fatal("expected ok")
	}
	if start != 1704081600 || end != 1704085200 {
		t.Fatalf("got (%d,%d)", start, end)
	}
}

func TestParseCatchupQueryDefaultsEnd(t *testing.T) {
	// 只给 start，end 应该是 max(start+60, min(start+7200, now))
	start, end, ok := ParseCatchupQuery("start=1000000000")
	if !ok {
		t.Fatal("expected ok")
	}
	if start != 1000000000 {
		t.Fatalf("start = %d", start)
	}
	if end != 1000000060 {
		t.Fatalf("end = %d, want start+60 (since now 远大于 start+7200)", end)
	}
}

func TestParseCatchupQueryRecentStart(t *testing.T) {
	// start 很近时，end 应被 now 截断
	now := time.Now().Unix()
	start, end, ok := ParseCatchupQuery("utc=" + itoa64(now-120))
	if !ok {
		t.Fatal("expected ok")
	}
	if end > now {
		t.Fatalf("end %d should not exceed now %d", end, now)
	}
	if end <= start {
		t.Fatalf("end %d should be > start %d", end, start)
	}
}

func itoa64(v int64) string {
	if v == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	return string(buf[i:])
}
