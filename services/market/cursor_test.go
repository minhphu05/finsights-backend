package market

import (
	"reflect"
	"testing"
)

func TestNewsCursorRoundTrip(t *testing.T) {
	for _, cursor := range []newsCursor{
		{publishedAt: "2026-11-24 22:25:00.000000", id: 42},
		{publishedAt: "2026-11-24 22:25:00.123000", id: 18446744073709551615},
		{id: 7},
	} {
		decoded, err := decodeNewsCursor(cursor.encode())
		if err != nil {
			t.Fatalf("decode %+v: %v", cursor, err)
		}
		if decoded != cursor {
			t.Fatalf("decoded %+v, want %+v", decoded, cursor)
		}
	}
}

func TestDecodeNewsCursorRejectsBadTokens(t *testing.T) {
	for name, token := range map[string]string{
		"not base64":   "%%%",
		"no separator": "NDI",
		"bad time":     newsCursor{publishedAt: "yesterday", id: 1}.encode(),
		"bad id":       "fGFiYw",
	} {
		if _, err := decodeNewsCursor(token); err == nil {
			t.Errorf("%s: token %q was accepted", name, token)
		}
	}
}

func TestNewsCursorAfter(t *testing.T) {
	clause, args := newsCursor{publishedAt: "2026-11-24 22:25:00.000000", id: 42}.after()
	if want := `(a.published_at<? OR (a.published_at=? AND a.id<?) OR a.published_at IS NULL)`; clause != want {
		t.Errorf("dated clause = %s", clause)
	}
	if want := []any{"2026-11-24 22:25:00.000000", "2026-11-24 22:25:00.000000", uint64(42)}; !reflect.DeepEqual(args, want) {
		t.Errorf("dated args = %v", args)
	}
	clause, args = newsCursor{id: 7}.after()
	if clause != `(a.published_at IS NULL AND a.id<?)` || !reflect.DeepEqual(args, []any{uint64(7)}) {
		t.Errorf("undated clause = %s, args = %v", clause, args)
	}
}
