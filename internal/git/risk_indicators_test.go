package git

import (
	"reflect"
	"testing"

	"deploy-api/internal/domain"
)

func TestComputeRiskIndicators_FlagsByPath(t *testing.T) {
	files := []string{
		"prisma/migrations/20261127_add_col/migration.sql",
		"src/devices/dto/create-device.dto.ts",
		"src/devices/devices.controller.ts",
		"proto/telemetry.proto",
		".env.example",
		"k8s/overlay-prod/kustomization.yaml",
		"src/devices/devices.service.ts",
		"src/devices/devices.controller.spec.ts",
	}
	ind := computeRiskIndicators(nil, files, nil)

	if !reflect.DeepEqual(ind.MigrationFiles, []string{"prisma/migrations/20261127_add_col/migration.sql"}) {
		t.Errorf("migration files: %v", ind.MigrationFiles)
	}
	wantContract := []string{"src/devices/dto/create-device.dto.ts", "src/devices/devices.controller.ts", "proto/telemetry.proto"}
	if !reflect.DeepEqual(ind.ContractFiles, wantContract) {
		t.Errorf("contract files: %v", ind.ContractFiles)
	}
	if !reflect.DeepEqual(ind.ConfigFiles, []string{".env.example", "k8s/overlay-prod/kustomization.yaml"}) {
		t.Errorf("config files: %v", ind.ConfigFiles)
	}
	if !ind.HasMigration || !ind.HasContractChange || !ind.HasConfigChange {
		t.Errorf("expected all three flags, got %+v", ind)
	}
}

func TestComputeRiskIndicators_IgnoresDiffBodyKeywords(t *testing.T) {
	diff := "diff --git a/src/x.service.ts b/src/x.service.ts\n" +
		"--- a/src/x.service.ts\n+++ b/src/x.service.ts\n" +
		"@@ -1 +1 @@\n-// read the database schema\n+// read the database schema from config.json\n"
	ind := computeRiskIndicators(nil, []string{"src/x.service.ts"}, addedEnvVars(diff))
	if ind.HasMigration || ind.HasConfigChange || ind.HasContractChange {
		t.Fatalf("expected no flags, got %+v", ind)
	}
}

func TestAddedEnvVars(t *testing.T) {
	diff := "diff --git a/src/a.ts b/src/a.ts\n" +
		"--- a/src/a.ts\n+++ b/src/a.ts\n" +
		"@@ -1,2 +1,3 @@\n" +
		"-const a = process.env.OLD_URL;\n" +
		"+const a = process.env.OLD_URL ?? '';\n" +
		"+const b = this.configService.get<string>('NEW_TOKEN');\n" +
		"+const c = process.env['BUCKET_NAME'];\n" +
		"diff --git a/main.go b/main.go\n" +
		"--- a/main.go\n+++ b/main.go\n" +
		"@@ -1 +1 @@\n+v := os.Getenv(\"GONDOLA_URL\")\n" +
		"diff --git a/src/a.spec.ts b/src/a.spec.ts\n" +
		"--- a/src/a.spec.ts\n+++ b/src/a.spec.ts\n" +
		"@@ -1 +1 @@\n+process.env.TEST_ONLY = '1';\n"

	got := addedEnvVars(diff)
	want := []string{"BUCKET_NAME", "GONDOLA_URL", "NEW_TOKEN"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	ind := computeRiskIndicators(nil, []string{"src/a.ts"}, got)
	if !ind.HasConfigChange {
		t.Fatalf("a new env read alone should flag config, got %+v", ind)
	}
}

func TestComputeRiskIndicators_SkipsNoise(t *testing.T) {
	files := []string{
		"internal/api/foo_test.go",
		"internal/api/foo.go",
		"messages/en.json",
		"tsconfig.json",
		"tsconfig.build.json",
		".eslintrc.json",
	}
	ind := computeRiskIndicators(nil, files, nil)
	if ind.TouchesSharedLib || ind.HasContractChange || ind.HasConfigChange {
		t.Fatalf("expected no flags, got %+v", ind)
	}

	ind = computeRiskIndicators(nil, []string{"libs/shared-utils/src/x.ts", "app/config/settings.yaml"}, nil)
	if !ind.TouchesSharedLib || !ind.HasConfigChange {
		t.Fatalf("expected shared lib and config, got %+v", ind)
	}
}

func TestComputeRiskIndicators_MigrationLayouts(t *testing.T) {
	for _, f := range []string{
		"alembic/versions/abc123_add_column.py",
		"src/database/1700000000000-AddColumn.ts",
		"db/changelog/2026-11-27-add-col.xml",
		"db/schema.rb",
		"ent/migrate/schema.go",
	} {
		if ind := computeRiskIndicators(nil, []string{f}, nil); !ind.HasMigration {
			t.Errorf("%s: expected migration", f)
		}
	}
	if ind := computeRiskIndicators(nil, []string{"src/users/users.service.ts"}, nil); ind.HasMigration {
		t.Errorf("service file flagged as migration")
	}
}

func TestComputeRiskIndicators_BreakingMarker(t *testing.T) {
	cases := map[string]bool{
		"remove console.log":                       false,
		"chore: delete unused file":                false,
		"feat(api)!: drop v1 endpoints":            true,
		"fix: x\n\nBREAKING CHANGE: renamed field": true,
		"refactor: bc-break in device payload":     true,
	}
	for msg, want := range cases {
		ind := computeRiskIndicators([]domain.CommitInfo{{Message: msg}}, nil, nil)
		if ind.HasBreakingChange != want {
			t.Errorf("%q: got %v, want %v", msg, ind.HasBreakingChange, want)
		}
	}
}

func TestAddedEnvVars_BodyLinesLookingLikeHeaders(t *testing.T) {
	diff := "diff --git a/src/a.ts b/src/a.ts\n" +
		"--- a/src/a.ts\n+++ b/src/a.ts\n" +
		"@@ -1,2 +1,2 @@\n" +
		"--- removed sql comment process.env.MOVED\n" +
		"+++i; const x = process.env.ADDED;\n" +
		"+const y = process.env.MOVED;\n" +
		"diff --git a/src/my tests/a.spec.ts b/src/my tests/a.spec.ts\n" +
		"--- a/src/my tests/a.spec.ts\n+++ b/src/my tests/a.spec.ts\n" +
		"@@ -1 +1 @@\n+process.env.TEST_ONLY = '1';\n"

	if got := addedEnvVars(diff); !reflect.DeepEqual(got, []string{"ADDED"}) {
		t.Fatalf("got %v, want [ADDED]", got)
	}
}
