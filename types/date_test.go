package types

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDateJSONRoundTrip(t *testing.T) {
	t.Parallel()
	d, err := ParseDate("2026-03-04")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"2026-03-04"` {
		t.Fatalf("marshal = %s", b)
	}
	var got Date
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.String() != "2026-03-04" {
		t.Fatalf("round trip = %s", got)
	}
	if _, err := json.Marshal(Date{}); err == nil {
		t.Fatal("zero date marshaled")
	}
	if _, err := (Date{}).Value(); err == nil {
		t.Fatal("zero date produced a driver value")
	}
	var cleared Date
	if err := json.Unmarshal([]byte("null"), &cleared); err != nil {
		t.Fatal(err)
	}
	if !cleared.IsZero() {
		t.Fatal("null should clear a Date")
	}
}

func TestDateAcceptsRFC3339(t *testing.T) {
	t.Parallel()
	var d Date
	if err := json.Unmarshal([]byte(`"2026-03-04T15:04:05Z"`), &d); err != nil {
		t.Fatal(err)
	}
	if d.String() != "2026-03-04" {
		t.Fatalf("date = %s", d)
	}
}

func TestDateScanTime(t *testing.T) {
	t.Parallel()
	var d Date
	src := time.Date(2026, 3, 4, 18, 30, 0, 0, time.FixedZone("X", 3600))
	if err := d.Scan(src); err != nil {
		t.Fatal(err)
	}
	if d.String() != "2026-03-04" {
		t.Fatalf("scan = %s", d)
	}
	v, err := d.Value()
	if err != nil {
		t.Fatal(err)
	}
	gotTime, ok := v.(time.Time)
	if !ok || !gotTime.Equal(time.Date(2026, 3, 4, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("value = %#v", v)
	}
}

func TestDateGORMRoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID   uint
		Born *Date `gorm:"type:date"`
	}
	if err := db.AutoMigrate(&row{}); err != nil {
		t.Fatal(err)
	}
	born, err := ParseDate("2026-03-04")
	if err != nil {
		t.Fatal(err)
	}
	stored := row{Born: &born}
	if err := db.Create(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var got row
	if err := db.First(&got, stored.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Born == nil || got.Born.String() != "2026-03-04" {
		t.Fatalf("born = %#v", got.Born)
	}
	blank := row{}
	if err := db.Create(&blank).Error; err != nil {
		t.Fatal(err)
	}
	var empty row
	if err := db.First(&empty, blank.ID).Error; err != nil {
		t.Fatal(err)
	}
	if empty.Born != nil {
		t.Fatalf("null born read back as %#v", empty.Born)
	}
}

func TestOptionalDateAndUUIDSchemasAllowNull(t *testing.T) {
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	type body struct {
		Born  *Date      `json:"born" format:"date" nullable:"true"`
		Due   Date       `json:"due" format:"date"`
		Token *uuid.UUID `json:"token" format:"uuid" nullable:"true"`
	}
	s := reg.Schema(reflect.TypeOf(body{}), false, "Body")
	born := s.Properties["born"]
	if born.Format != "date" || !born.Nullable {
		t.Fatalf("born schema = type %q format %q nullable %v", born.Type, born.Format, born.Nullable)
	}
	due := s.Properties["due"]
	if due.Format != "date" || due.Nullable {
		t.Fatalf("due schema = type %q format %q nullable %v", due.Type, due.Format, due.Nullable)
	}
	token := s.Properties["token"]
	if token.Format != "uuid" || !token.Nullable {
		t.Fatalf("token schema = type %q format %q nullable %v", token.Type, token.Format, token.Nullable)
	}
	res := huma.ValidateResult{}
	huma.Validate(reg, s, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{
		"born": nil, "due": "2026-03-04", "token": nil,
	}, &res)
	if len(res.Errors) != 0 {
		t.Fatalf("null optional fields: %v", res.Errors)
	}
	res = huma.ValidateResult{}
	huma.Validate(reg, s, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{
		"born": "", "due": "2026-03-04", "token": "",
	}, &res)
	if len(res.Errors) == 0 {
		t.Fatal("empty date and uuid strings were accepted")
	}
}
