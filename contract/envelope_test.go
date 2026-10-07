package contract

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestDataJSONShape(t *testing.T) {
	t.Parallel()

	body := Data[string]{Data: "ok"}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	want := `{"data":"ok"}`
	if string(raw) != want {
		t.Fatalf("json = %s, want %s", raw, want)
	}
	if strings.Contains(string(raw), `"meta"`) {
		t.Fatalf("Data must not emit meta; got %s", raw)
	}
}

func TestDataMetaPageJSONShape(t *testing.T) {
	t.Parallel()

	body := DataMeta[[]string, PageMeta]{
		Data: []string{"a", "b"},
		Meta: &PageMeta{Page: 1, PerPage: 20, Total: 125},
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	data, ok := got["data"].([]any)
	if !ok || len(data) != 2 || data[0] != "a" || data[1] != "b" {
		t.Fatalf("data = %#v", got["data"])
	}
	meta, ok := got["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta missing: %#v", got)
	}
	wantMeta := map[string]any{
		"page":     float64(1),
		"per_page": float64(20),
		"total":    float64(125),
	}
	if !reflect.DeepEqual(meta, wantMeta) {
		t.Fatalf("meta = %#v, want %#v", meta, wantMeta)
	}
}

func TestDataMetaOmitsNilMeta(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(DataMeta[string, PageMeta]{Data: "ok"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(raw) != `{"data":"ok"}` {
		t.Fatalf("json = %s, want data-only when meta is nil", raw)
	}
}

func TestDataMetaZeroPageMetaStillSerializes(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(DataMeta[string, PageMeta]{
		Data: "ok",
		Meta: &PageMeta{},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(raw), `"meta"`) {
		t.Fatalf("non-nil zero PageMeta should serialize; got %s", raw)
	}
}

func TestClampPage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		page, perPage     int
		wantPage, wantPer int
	}{
		{0, 0, DefaultPage, DefaultPerPage},
		{1, 20, 1, 20},
		{2, 50, 2, 50},
		{3, 100, 3, 100},
		{1, 101, 1, MaxPerPage},
		{-5, -10, DefaultPage, DefaultPerPage},
		{0, 1000, DefaultPage, MaxPerPage},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("page=%d,per_page=%d", tt.page, tt.perPage), func(t *testing.T) {
			t.Parallel()
			gotPage, gotPer := ClampPage(tt.page, tt.perPage)
			if gotPage != tt.wantPage || gotPer != tt.wantPer {
				t.Fatalf("ClampPage(%d, %d) = (%d, %d), want (%d, %d)",
					tt.page, tt.perPage, gotPage, gotPer, tt.wantPage, tt.wantPer)
			}
		})
	}
}

func TestPageOffset(t *testing.T) {
	t.Parallel()

	tests := []struct {
		page, perPage, want int
	}{
		{1, 20, 0},
		{2, 20, 20},
		{3, 10, 20},
		// An offset that would overflow saturates instead of wrapping to a
		// negative number GORM reads as "no offset" (issue #441).
		{math.MaxInt, 100, math.MaxInt},
		{math.MaxInt/100 + 2, 100, math.MaxInt},
		{math.MaxInt/100 + 1, 100, math.MaxInt / 100 * 100},
		{math.MaxInt, 1, math.MaxInt - 1},
		// Unclamped input never panics or goes negative.
		{0, 20, 0},
		{-5, 20, 0},
		{3, 0, 0},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("page=%d,per_page=%d", tt.page, tt.perPage), func(t *testing.T) {
			t.Parallel()
			if got := PageOffset(tt.page, tt.perPage); got != tt.want {
				t.Fatalf("PageOffset(%d, %d) = %d, want %d", tt.page, tt.perPage, got, tt.want)
			}
		})
	}
}
