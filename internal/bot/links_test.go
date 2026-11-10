package bot

import (
	"reflect"
	"testing"
)

// TestExtractLinks holds table cases mirroring the semantics of the old Python extract_links.
// Every case is a pure function call with no network access.
func TestExtractLinks(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			name:  "single link without punctuation",
			input: "https://t.me/durov/123",
			want:  []string{"https://t.me/durov/123"},
		},
		{
			name:  "CJK full stop right after the link",
			input: "看这个 https://t.me/xxx/123。",
			want:  []string{"https://t.me/xxx/123"},
		},
		{
			name:  "fullwidth comma right after the link",
			input: "先看 https://t.me/xxx/1，再看下一条",
			want:  []string{"https://t.me/xxx/1"},
		},
		{
			name:  "fullwidth enumeration comma right after the link",
			input: "https://t.me/xxx/2、https://t.me/xxx/3",
			want:  []string{"https://t.me/xxx/2", "https://t.me/xxx/3"},
		},
		{
			name:  "fullwidth semicolon right after the link",
			input: "链接A https://t.me/xxx/4；链接B https://t.me/xxx/5",
			want:  []string{"https://t.me/xxx/4", "https://t.me/xxx/5"},
		},
		{
			name:  "fullwidth colon right after the link",
			input: "地址如下：https://t.me/xxx/6：",
			want:  []string{"https://t.me/xxx/6"},
		},
		{
			name:  "fullwidth exclamation mark right after the link",
			input: "快看 https://t.me/xxx/7！",
			want:  []string{"https://t.me/xxx/7"},
		},
		{
			name:  "fullwidth question mark right after the link",
			input: "这是 https://t.me/xxx/8？",
			want:  []string{"https://t.me/xxx/8"},
		},
		{
			name:  "link wrapped in fullwidth parentheses",
			input: "（https://t.me/xxx/9）",
			want:  []string{"https://t.me/xxx/9"},
		},
		{
			name:  "link wrapped in fullwidth square brackets",
			input: "【https://t.me/xxx/10】",
			want:  []string{"https://t.me/xxx/10"},
		},
		{
			name:  "link wrapped in book-title marks",
			input: "《https://t.me/xxx/11》",
			want:  []string{"https://t.me/xxx/11"},
		},
		{
			name:  "ASCII period stripped",
			input: "https://t.me/a/1.",
			want:  []string{"https://t.me/a/1"},
		},
		{
			name:  "ASCII comma stripped",
			input: "https://t.me/a/2,",
			want:  []string{"https://t.me/a/2"},
		},
		{
			name:  "ASCII semicolon stripped",
			input: "https://t.me/a/3;",
			want:  []string{"https://t.me/a/3"},
		},
		{
			name:  "ASCII colon stripped",
			input: "https://t.me/a/4:",
			want:  []string{"https://t.me/a/4"},
		},
		{
			name:  "ASCII exclamation mark stripped",
			input: "https://t.me/a/5!",
			want:  []string{"https://t.me/a/5"},
		},
		{
			name:  "ASCII question mark stripped",
			input: "https://t.me/a/6?",
			want:  []string{"https://t.me/a/6"},
		},
		{
			name:  "ASCII closing parenthesis stripped",
			input: "看 (https://t.me/a/7)",
			want:  []string{"https://t.me/a/7"},
		},
		{
			name:  "ASCII double quote stripped",
			input: "\"https://t.me/a/8\"",
			want:  []string{"https://t.me/a/8"},
		},
		{
			name:  "ASCII single quote stripped",
			input: "'https://t.me/a/9'",
			want:  []string{"https://t.me/a/9"},
		},
		{
			name:  "ASCII angle brackets stripped",
			input: "<https://t.me/a/10>",
			want:  []string{"https://t.me/a/10"},
		},
		{
			name:  "ASCII closing square bracket stripped",
			input: "https://t.me/a/11]",
			want:  []string{"https://t.me/a/11"},
		},
		{
			name:  "ASCII closing brace stripped",
			input: "https://t.me/a/12}",
			want:  []string{"https://t.me/a/12"},
		},
		{
			name:  "bare link without a scheme prefix",
			input: "t.me/music_tag_web/44",
			want:  []string{"t.me/music_tag_web/44"},
		},
		{
			name:  "bare link followed by CJK punctuation",
			input: "看这个 t.me/music_tag_web/44。",
			want:  []string{"t.me/music_tag_web/44"},
		},
		{
			name:  "bare link embedded in CJK text",
			input: "分享一个t.me/durov/552好看",
			want:  []string{"t.me/durov/552"},
		},
		{
			name:  "http scheme supported too",
			input: "http://t.me/a/1",
			want:  []string{"http://t.me/a/1"},
		},
		{
			name:  "t.me inside a domain is not matched",
			input: "example.com/t.me/x",
			want:  []string{},
		},
		{
			name:  "t.me inside a subdomain is not matched",
			input: "foo.t.me/x",
			want:  []string{},
		},
		{
			name:  "three links keep their order",
			input: "一 https://t.me/x/1 二 https://t.me/y/2 三 https://t.me/z/3",
			want:  []string{"https://t.me/x/1", "https://t.me/y/2", "https://t.me/z/3"},
		},
		{
			name:  "duplicate links deduplicated at the first position",
			input: "https://t.me/x/1 再看 https://t.me/x/1 还有 https://t.me/x/1",
			want:  []string{"https://t.me/x/1"},
		},
		{
			name:  "duplicates after stripping are deduplicated too",
			input: "https://t.me/x/1。 https://t.me/x/1.",
			want:  []string{"https://t.me/x/1"},
		},
		{
			name:  "text without links",
			input: "今天天气不错，出来走走吧",
			want:  []string{},
		},
		{
			name:  "empty string",
			input: "",
			want:  []string{},
		},
		{
			name:  "pure CJK text",
			input: "这是一条没有任何链接的纯中文消息。",
			want:  []string{},
		},
		{
			name:  "non-t.me link is not matched",
			input: "文档在 https://example.com/a 参考",
			want:  []string{},
		},
		{
			name:  "lookalike domain is not matched",
			input: "钓鱼 https://t.me.evil.com/x/1 别点",
			want:  []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractLinks(tt.input)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("extractLinks(%q) = %#v, want %#v", tt.input, got, tt.want)
			}
		})
	}
}
