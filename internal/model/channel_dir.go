package model

import (
	"strings"
)

// LogoBase 频道台标 CDN 前缀（沿用原项目地址）。
const LogoBase = "https://garysclub.sharewithyou.dpdns.org/logos/ysp-live-logos"

// True4KChannels 真正的 4K/8K 频道。
var True4KChannels = map[string]bool{
	"cctv4k":   true,
	"cctv8k":   true,
	"cctv164k": true,
}

// ChannelMeta 频道目录中的一条记录。
type ChannelMeta struct {
	Slug  string `json:"slug"`
	Name  string `json:"name"`
	SID   string `json:"sid"`
	PID   string `json:"pid"`
	Defn  string `json:"defn"`
	Logo  string `json:"logo"`
	Group string `json:"group"`
	TVGID string `json:"tvg_id"`
}

// ChannelDirectory 频道目录（原 global_channel_dir）。
type ChannelDirectory struct {
	metas     map[string]*ChannelMeta
	canonical []string
}

// ChannelGroupOf 返回频道分组：央视 / 卫视。
func ChannelGroupOf(slug string) string {
	s := strings.ToLower(strings.TrimSpace(slug))
	if alias, ok := AliasMap[s]; ok {
		s = alias
	}
	if strings.HasPrefix(s, "cctv") || strings.HasPrefix(s, "cgtn") {
		return "央视"
	}
	return "卫视"
}

// NewChannelDirectory 构建频道目录。
func NewChannelDirectory() *ChannelDirectory {
	d := &ChannelDirectory{metas: make(map[string]*ChannelMeta, len(DefaultChannels))}
	for _, c := range DefaultChannels {
		tid, ok := TVGIDs[c.Slug]
		if !ok {
			tid = strings.ToUpper(c.Slug)
		}
		d.metas[c.Slug] = &ChannelMeta{
			Slug:  c.Slug,
			Name:  c.Name,
			SID:   c.SID,
			PID:   c.PID,
			Defn:  c.Defn,
			Logo:  LogoBase + "/" + c.Slug + ".png",
			Group: ChannelGroupOf(c.Slug),
			TVGID: tid,
		}
		d.canonical = append(d.canonical, c.Slug)
	}
	return d
}

// NormalizeChannel 归一化频道名（去空格/斜杠/.m3u8，处理 cctv16-4k 等写法）。
func NormalizeChannel(raw string) string {
	key := strings.ToLower(strings.Trim(strings.TrimSpace(raw), "/"))
	key = strings.TrimSuffix(key, ".m3u8")
	switch key {
	case "cctv16-4k", "cctv16_4k", "cctv16/4k", "cctv16(4k)":
		return "cctv164k"
	}
	return key
}

// Canonical 返回规范 slug（应用别名映射）。
func Canonical(slug string) string {
	s := NormalizeChannel(slug)
	if alias, ok := AliasMap[s]; ok {
		return alias
	}
	return s
}

// GetMeta 按 slug（支持别名）取频道元数据。
func (d *ChannelDirectory) GetMeta(slug string) *ChannelMeta {
	return d.metas[Canonical(slug)]
}

// AllChannels 返回全部频道元数据（按默认顺序）。
func (d *ChannelDirectory) AllChannels() []*ChannelMeta {
	out := make([]*ChannelMeta, 0, len(d.canonical))
	for _, s := range d.canonical {
		if m, ok := d.metas[s]; ok {
			out = append(out, m)
		}
	}
	return out
}

// Count 频道总数。
func (d *ChannelDirectory) Count() int { return len(d.canonical) }
