package types

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestDurationJSONRoundTrip(t *testing.T) {
	t.Parallel()
	span, err := ParseDuration("1h30m")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(span)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"1h30m0s"` {
		t.Fatalf("marshal = %s", b)
	}
	var got Duration
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.Duration() != 90*time.Minute {
		t.Fatalf("round trip = %s", got)
	}
	zero, err := json.Marshal(Duration{})
	if err != nil || string(zero) != `"0s"` {
		t.Fatalf("zero = %s, err %v", zero, err)
	}
	if _, err := ParseDuration(""); err == nil {
		t.Fatal("empty duration was accepted")
	}
	neg, err := ParseDuration("-1s")
	if err != nil || neg.Duration() != -time.Second {
		t.Fatalf("negative = %s, err %v", neg, err)
	}
}

func TestDurationGORMRoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID    uint
		Break *Duration `gorm:"type:bigint"`
		Shift Duration  `gorm:"type:bigint;not null"`
	}
	if err := db.AutoMigrate(&row{}); err != nil {
		t.Fatal(err)
	}
	span := MustDuration("45m")
	stored := row{Break: &span, Shift: MustDuration("0s")}
	if err := db.Create(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var got row
	if err := db.First(&got, stored.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Break == nil || got.Break.Duration() != 45*time.Minute || got.Shift.Duration() != 0 {
		t.Fatalf("row = %#v break %v shift %v", got, got.Break, got.Shift.Duration())
	}
	blank := row{Shift: MustDuration("1s")}
	if err := db.Create(&blank).Error; err != nil {
		t.Fatal(err)
	}
	var empty row
	if err := db.First(&empty, blank.ID).Error; err != nil {
		t.Fatal(err)
	}
	if empty.Break != nil {
		t.Fatalf("null break read back as %#v", empty.Break)
	}
}

func TestDurationSchemaRejectsBlank(t *testing.T) {
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	type body struct {
		Break *Duration `json:"break" format:"duration" nullable:"true"`
		Shift Duration  `json:"shift" format:"duration"`
	}
	s := reg.Schema(reflect.TypeOf(body{}), false, "Body")
	optional := s.Properties["break"]
	if optional.Format != "duration" || !optional.Nullable {
		t.Fatalf("break schema = type %q format %q nullable %v", optional.Type, optional.Format, optional.Nullable)
	}
	res := huma.ValidateResult{}
	huma.Validate(reg, s, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{
		"break": nil, "shift": "30m",
	}, &res)
	if len(res.Errors) != 0 {
		t.Fatalf("null optional duration: %v", res.Errors)
	}
	res = huma.ValidateResult{}
	huma.Validate(reg, s, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{
		"break": "", "shift": "30m",
	}, &res)
	if len(res.Errors) == 0 {
		t.Fatal("empty duration was accepted")
	}
}
