package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gombit-dev/gombit/config"
	"github.com/gombit-dev/gombit/migrations/schemaplan"
)

func samplePlan() schemaplan.SchemaPlan {
	return schemaplan.SchemaPlan{
		Driver: config.DatabaseDriverSQLite,
		Steps: []schemaplan.PlanStep{
			{ID: "drop_column:products.name", Code: schemaplan.StepDropColumn, Severity: schemaplan.SeverityDestructive, Table: "products", Name: "name", Detail: "Drops column products.name and the data in it."},
			{ID: "add_index:products.idx_sku", Code: schemaplan.StepAddIndex, Severity: schemaplan.SeveritySafe, Table: "products", Name: "idx_sku", Detail: "Adds index idx_sku."},
		},
	}
}

func TestWritePlanResultFailsOnUnacknowledgedStep(t *testing.T) {
	var out bytes.Buffer
	err := writePlanResult(&out, samplePlan(), false)
	if err == nil || !strings.Contains(err.Error(), "1 destructive or unsafe change(s) need acknowledgement") {
		t.Fatalf("writePlanResult() error = %v, want the acknowledgement failure", err)
	}
	if !strings.Contains(out.String(), "--allow drop_column:products.name") {
		t.Fatalf("output missing the --allow line:\n%s", out.String())
	}
}

func TestWritePlanResultPassesOnceAllowed(t *testing.T) {
	plan := samplePlan()
	plan.Acknowledge([]string{"drop_column"})
	var out bytes.Buffer
	if err := writePlanResult(&out, plan, false); err != nil {
		t.Fatalf("writePlanResult() error = %v, want nil once the drop is allowed", err)
	}
	if !strings.Contains(out.String(), "(allowed)") {
		t.Fatalf("output does not mark the allowed step:\n%s", out.String())
	}
}

func TestWritePlanResultJSON(t *testing.T) {
	var out bytes.Buffer
	err := writePlanResult(&out, samplePlan(), true)
	if err == nil {
		t.Fatal("writePlanResult(--json) error = nil, want the acknowledgement failure")
	}
	var got planJSON
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	if !got.NeedsAcknowledgement || len(got.UnacknowledgedStepIDs) != 1 || got.UnacknowledgedStepIDs[0] != "drop_column:products.name" {
		t.Fatalf("JSON = %+v, want one unacknowledged drop_column", got)
	}
	if len(got.Steps) != 2 || got.Steps[1].Severity != schemaplan.SeveritySafe {
		t.Fatalf("JSON steps = %+v, want both steps with severities", got.Steps)
	}
}

func TestPlanHelpDescribesSeveritiesAndAllow(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{"db", "plan", "--help"}); err != nil {
		t.Fatalf("gombit db plan --help: %v", err)
	}
	out := stdout.String() + stderr.String()
	for _, want := range []string{"destructive", "unsafe", "--allow", "--json", "possible rename"} {
		if !strings.Contains(out, want) {
			t.Fatalf("help missing %q:\n%s", want, out)
		}
	}
}

func TestMakeMigrationsHelpDescribesAllow(t *testing.T) {
	stdout := new(bytes.Buffer)
	stderr := new(bytes.Buffer)
	if err := ExecuteRoot(context.Background(), NewRoot(stdout, stderr), []string{"db", "makemigrations", "--help"}); err != nil {
		t.Fatalf("gombit db makemigrations --help: %v", err)
	}
	if out := stdout.String() + stderr.String(); !strings.Contains(out, "--allow") {
		t.Fatalf("help missing --allow:\n%s", out)
	}
}
