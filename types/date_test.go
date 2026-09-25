package types

import (
	"encoding/json"
	"testing"
	"time"
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
	var zero Date
	b, err = json.Marshal(zero)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "null" {
		t.Fatalf("zero marshal = %s", b)
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
	if v != "2026-03-04" {
		t.Fatalf("value = %#v", v)
	}
}
