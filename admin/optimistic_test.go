package admin_test

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/gombit-dev/gombit/admin"
)

// TestResourceOptimisticLocking covers #224: a model with an integer "version"
// column gets optimistic-locking on the admin update path. A PATCH guarded on a
// stale version is rejected with 409 instead of silently last-write-wins, and a
// PATCH on the current version succeeds and bumps the version.
func TestResourceOptimisticLocking(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Account struct {
		ID      uint `gorm:"primaryKey" json:"id"`
		Balance int  `json:"balance"`
		Version int  `json:"version"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&Account{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, Account{}, admin.Options{
		Slug: "accounts",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "balance", Type: admin.TypeInteger},
			{Name: "version", Type: admin.TypeInteger, ReadOnly: true},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	acct := Account{Balance: 100, Version: 1}
	if err := app.DB().Create(&acct).Error; err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	jar := loginSuperuser(t, app)
	path := fmt.Sprintf("/api/v1/admin/resources/accounts/%d", acct.ID)

	// First writer, based on version 1, wins and bumps to 2.
	first := doRequest(app, jar, http.MethodPatch, path, `{"balance":110,"version":1}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first patch status = %d; body: %s", first.Code, first.Body.String())
	}
	var firstBody struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &firstBody); err != nil {
		t.Fatalf("decode first: %v", err)
	}
	if got := asInt(firstBody.Data["version"]); got != 2 {
		t.Fatalf("version after first patch = %d, want 2", got)
	}

	// Second writer still based on the stale version 1 must lose with 409.
	stale := doRequest(app, jar, http.MethodPatch, path, `{"balance":120,"version":1}`)
	assertError(t, stale, http.StatusConflict, "conflict")

	// The stale write must not have applied.
	var stored Account
	if err := app.DB().First(&stored, acct.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Balance != 110 || stored.Version != 2 {
		t.Fatalf("stored = %+v, want balance 110 version 2 (stale write must not win)", stored)
	}

	// A writer on the current version 2 succeeds again.
	ok := doRequest(app, jar, http.MethodPatch, path, `{"balance":130,"version":2}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("current-version patch status = %d; body: %s", ok.Code, ok.Body.String())
	}
	if err := app.DB().First(&stored, acct.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Balance != 130 || stored.Version != 3 {
		t.Fatalf("stored = %+v, want balance 130 version 3", stored)
	}
}

// TestResourceOptimisticLockingWithoutClientVersion verifies the loaded-version
// guard: even when the PATCH omits "version", the update still bumps the column
// so a genuinely concurrent writer would be rejected. Here we assert the happy
// path (no client version supplied) still increments.
func TestResourceOptimisticLockingWithoutClientVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	type Ledger struct {
		ID      uint `gorm:"primaryKey" json:"id"`
		Total   int  `json:"total"`
		Version int  `json:"version"`
	}
	app := newCookieApp(t)
	if err := app.DB().AutoMigrate(&Ledger{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	if err := admin.Register(app, Ledger{}, admin.Options{
		Slug: "ledgers",
		Fields: []admin.Field{
			{Name: "id", Type: admin.TypeInteger, ReadOnly: true},
			{Name: "total", Type: admin.TypeInteger},
			{Name: "version", Type: admin.TypeInteger, ReadOnly: true},
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	row := Ledger{Total: 5, Version: 7}
	if err := app.DB().Create(&row).Error; err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	jar := loginSuperuser(t, app)
	path := fmt.Sprintf("/api/v1/admin/resources/ledgers/%d", row.ID)

	resp := doRequest(app, jar, http.MethodPatch, path, `{"total":9}`)
	if resp.Code != http.StatusOK {
		t.Fatalf("patch status = %d; body: %s", resp.Code, resp.Body.String())
	}
	var stored Ledger
	if err := app.DB().First(&stored, row.ID).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}
	if stored.Total != 9 || stored.Version != 8 {
		t.Fatalf("stored = %+v, want total 9 version 8 (loaded-version guard bumps)", stored)
	}
}
