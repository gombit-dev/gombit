package types

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestTimeOfDayJSONRoundTrip(t *testing.T) {
	t.Parallel()
	clock, err := ParseTimeOfDay("15:04:05")
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(clock)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `"15:04:05"` {
		t.Fatalf("marshal = %s", b)
	}
	var got TimeOfDay
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got.String() != "15:04:05" {
		t.Fatalf("round trip = %s", got)
	}
	midnight := TimeOfDay{}
	b, err = json.Marshal(midnight)
	if err != nil || string(b) != `"00:00:00"` {
		t.Fatalf("midnight = %s, err %v", b, err)
	}
	if _, err := json.Marshal((*TimeOfDay)(nil)); err != nil {
		t.Fatal(err)
	}
}

func TestTimeOfDayAcceptsShortAndOffset(t *testing.T) {
	t.Parallel()
	short, err := ParseTimeOfDay("09:05")
	if err != nil || short.String() != "09:05:00" {
		t.Fatalf("short = %s, err %v", short, err)
	}
	zoned, err := ParseTimeOfDay("15:04:05+07:00")
	if err != nil || zoned.String() != "15:04:05" {
		t.Fatalf("zoned = %s, err %v", zoned, err)
	}
	if _, err := ParseTimeOfDay(""); err == nil {
		t.Fatal("empty time of day was accepted")
	}
	if _, err := ParseTimeOfDay("24:00:00"); err == nil {
		t.Fatal("24:00:00 was accepted")
	}
}

func TestTimeOfDayGORMRoundTrip(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	type row struct {
		ID    uint
		Opens *TimeOfDay `gorm:"type:char(8)"`
		Shift TimeOfDay  `gorm:"type:char(8);not null"`
	}
	if err := db.AutoMigrate(&row{}); err != nil {
		t.Fatal(err)
	}
	opens, err := ParseTimeOfDay("08:30:00")
	if err != nil {
		t.Fatal(err)
	}
	stored := row{Opens: &opens, Shift: MustTimeOfDay("00:00:00")}
	if err := db.Create(&stored).Error; err != nil {
		t.Fatal(err)
	}
	var got row
	if err := db.First(&got, stored.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Opens == nil || got.Opens.String() != "08:30:00" || got.Shift.String() != "00:00:00" {
		t.Fatalf("row = %#v", got)
	}
	blank := row{Shift: MustTimeOfDay("12:00:00")}
	if err := db.Create(&blank).Error; err != nil {
		t.Fatal(err)
	}
	var empty row
	if err := db.First(&empty, blank.ID).Error; err != nil {
		t.Fatal(err)
	}
	if empty.Opens != nil {
		t.Fatalf("null opens read back as %#v", empty.Opens)
	}
}

func TestTimeOfDaySchemaRejectsBlank(t *testing.T) {
	reg := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	type body struct {
		Opens *TimeOfDay `json:"opens" format:"time" nullable:"true"`
		Shift TimeOfDay  `json:"shift" format:"time"`
	}
	s := reg.Schema(reflect.TypeOf(body{}), false, "Body")
	opens := s.Properties["opens"]
	if opens.Format != "time" || !opens.Nullable {
		t.Fatalf("opens schema = type %q format %q nullable %v", opens.Type, opens.Format, opens.Nullable)
	}
	res := huma.ValidateResult{}
	huma.Validate(reg, s, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{
		"opens": nil, "shift": "15:04:05",
	}, &res)
	if len(res.Errors) != 0 {
		t.Fatalf("null optional time: %v", res.Errors)
	}
	res = huma.ValidateResult{}
	huma.Validate(reg, s, &huma.PathBuffer{}, huma.ModeWriteToServer, map[string]any{
		"opens": "", "shift": "15:04:05",
	}, &res)
	if len(res.Errors) == 0 {
		t.Fatal("empty time of day was accepted")
	}
}
