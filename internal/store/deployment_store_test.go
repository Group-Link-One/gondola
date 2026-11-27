package store

import (
	"testing"
	"time"

	"deploy-api/internal/domain"
)

func TestDeploymentStore_Create(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertTestBuild(t, db, 1, "test-app")
	insertTestBuild(t, db, 2, "test-app")
	store := NewDeploymentStore(db)

	prevBuildID := int64(1)
	deployment := &domain.Deployment{
		AppName:         "test-app",
		Environment:     domain.EnvStaging,
		BuildID:         2,
		PreviousBuildID: &prevBuildID,
		DeployedAt:      time.Now(),
		DeployedBy:      "test-user",
		Status:          domain.StatusPending,
	}

	id, err := store.Create(deployment)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if id <= 0 {
		t.Errorf("Expected positive ID, got %d", id)
	}
}

func TestDeploymentStore_GetByID(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertTestBuild(t, db, 1, "test-app")
	store := NewDeploymentStore(db)

	deployment := &domain.Deployment{
		AppName:     "test-app",
		Environment: domain.EnvProd,
		BuildID:     1,
		DeployedAt:  time.Now(),
		DeployedBy:  "api",
		Status:      domain.StatusPushed,
	}

	id, err := store.Create(deployment)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	retrieved, err := store.GetByID(id)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}

	if retrieved == nil {
		t.Fatal("Expected deployment, got nil")
	}

	if retrieved.AppName != "test-app" {
		t.Errorf("Expected AppName test-app, got %s", retrieved.AppName)
	}

	if retrieved.Status != deployment.Status {
		t.Errorf("Expected Status %s, got %s", deployment.Status, retrieved.Status)
	}
}

func TestDeploymentStore_UpdateStatus(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertTestBuild(t, db, 1, "test-app")
	store := NewDeploymentStore(db)

	deployment := &domain.Deployment{
		AppName:     "test-app",
		Environment: domain.EnvStaging,
		BuildID:     1,
		DeployedAt:  time.Now(),
		DeployedBy:  "api",
		Status:      domain.StatusPending,
	}

	id, err := store.Create(deployment)
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	// Update status
	err = store.UpdateStatus(id, domain.StatusPushed, "")
	if err != nil {
		t.Fatalf("UpdateStatus failed: %v", err)
	}

	// Verify
	retrieved, err := store.GetByID(id)
	if err != nil {
		t.Fatalf("GetByID failed: %v", err)
	}

	if retrieved.Status != domain.StatusPushed {
		t.Errorf("Expected status %s, got %s", domain.StatusPushed, retrieved.Status)
	}
}

func TestDeploymentStore_List(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertTestBuild(t, db, 1, "app-1")
	insertTestBuild(t, db, 2, "app-2")
	store := NewDeploymentStore(db)

	// Create deployments for different apps and environments
	deployments := []struct {
		App         string
		Environment domain.Environment
		Status      domain.DeploymentStatus
		BuildID     int64
	}{
		{"app-1", domain.EnvStaging, domain.StatusPushed, 1},
		{"app-1", domain.EnvProd, domain.StatusPushed, 1},
		{"app-2", domain.EnvStaging, domain.StatusPushed, 2},
		{"app-1", domain.EnvStaging, domain.StatusFailed, 1},
	}

	for _, d := range deployments {
		dep := &domain.Deployment{
			AppName:     d.App,
			Environment: d.Environment,
			BuildID:     d.BuildID,
			DeployedAt:  time.Now(),
			DeployedBy:  "test",
			Status:      d.Status,
		}
		_, err := store.Create(dep)
		if err != nil {
			t.Fatalf("Create failed: %v", err)
		}
	}

	// List all for app-1
	filter := domain.DeploymentFilter{AppName: "app-1", Limit: 10}
	list, err := store.List(filter)
	if err != nil {
		t.Fatalf("List failed: %v", err)
	}

	if len(list) != 3 {
		t.Errorf("Expected 3 deployments for app-1, got %d", len(list))
	}

	// List for app-1 staging only
	filter.Environment = domain.EnvStaging
	list, err = store.List(filter)
	if err != nil {
		t.Fatalf("List with env filter failed: %v", err)
	}

	if len(list) != 2 {
		t.Errorf("Expected 2 deployments for app-1 staging, got %d", len(list))
	}
}

func TestDeploymentStore_GetLatestDeployment(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	insertTestBuild(t, db, 1, "test-app")
	insertTestBuild(t, db, 2, "test-app")
	store := NewDeploymentStore(db)

	// Create two deployments, second one is later
	dep1 := &domain.Deployment{
		AppName:     "test-app",
		Environment: domain.EnvProd,
		BuildID:     1,
		DeployedAt:  time.Now().Add(-time.Hour),
		DeployedBy:  "test",
		Status:      domain.StatusPushed,
	}

	_, err := store.Create(dep1)
	if err != nil {
		t.Fatalf("Create dep1 failed: %v", err)
	}

	dep2 := &domain.Deployment{
		AppName:     "test-app",
		Environment: domain.EnvProd,
		BuildID:     2,
		DeployedAt:  time.Now(),
		DeployedBy:  "test",
		Status:      domain.StatusPushed,
	}

	_, err = store.Create(dep2)
	if err != nil {
		t.Fatalf("Create dep2 failed: %v", err)
	}

	latest, err := store.GetLatestDeployment("test-app", domain.EnvProd)
	if err != nil {
		t.Fatalf("GetLatestDeployment failed: %v", err)
	}

	if latest == nil {
		t.Fatal("Expected deployment, got nil")
	}

	// latest should be build 2 (CommitSHA from insertTestBuild is "sha2")
	if latest.CommitSHA != "sha2" {
		t.Errorf("Expected latest commit sha2, got %s", latest.CommitSHA)
	}
}

func TestDeploymentStore_GetAllCurrentDeployments(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	// Builds carry image_tag "v<id>" (see insertTestBuild).
	insertTestBuild(t, db, 1, "app-1") // v1
	insertTestBuild(t, db, 2, "app-1") // v2 (supersedes v1 on staging)
	insertTestBuild(t, db, 3, "app-2") // v3
	store := NewDeploymentStore(db)

	now := time.Now()
	deployments := []struct {
		Env        domain.Environment
		BuildID    int64
		Status     domain.DeploymentStatus
		DeployedAt time.Time
	}{
		// app-1 staging: older push then a newer push -> newer (v2) wins.
		{domain.EnvStaging, 1, domain.StatusPushed, now.Add(-2 * time.Hour)},
		{domain.EnvStaging, 2, domain.StatusPushed, now.Add(-1 * time.Hour)},
		// app-1 prod: single live deployment (v1).
		{domain.EnvProd, 1, domain.StatusPushed, now.Add(-3 * time.Hour)},
		// app-2 staging: a live deployment (v3) plus a later FAILED one that
		// must be ignored (not live).
		{domain.EnvStaging, 3, domain.StatusPushed, now.Add(-90 * time.Minute)},
		{domain.EnvStaging, 3, domain.StatusFailed, now.Add(-1 * time.Minute)},
		// app-2 prod: only ever pending -> not live, must not appear.
		{domain.EnvProd, 3, domain.StatusPending, now},
	}

	for _, d := range deployments {
		dep := &domain.Deployment{
			Environment: d.Env,
			BuildID:     d.BuildID,
			DeployedAt:  d.DeployedAt,
			DeployedBy:  "test",
			Status:      d.Status,
		}
		if _, err := store.Create(dep); err != nil {
			t.Fatalf("Create failed: %v", err)
		}
	}

	current, err := store.GetAllCurrentDeployments()
	if err != nil {
		t.Fatalf("GetAllCurrentDeployments failed: %v", err)
	}

	// Expect exactly: app-1/staging=v2, app-1/prod=v1, app-2/staging=v3.
	got := make(map[string]string) // "app|env" -> image_tag
	for _, d := range current {
		got[string(d.AppName)+"|"+string(d.Environment)] = d.ImageTag
	}

	want := map[string]string{
		"app-1|staging": "v2",
		"app-1|prod":    "v1",
		"app-2|staging": "v3",
	}

	if len(got) != len(want) {
		t.Fatalf("Expected %d current deployments, got %d: %#v", len(want), len(got), got)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("For %s expected image_tag %q, got %q", k, v, got[k])
		}
	}
}

func TestDeploymentStore_GetLatestDeployment_NoDeployments(t *testing.T) {
	db, cleanup := setupTestDB(t)
	defer cleanup()

	store := NewDeploymentStore(db)

	latest, err := store.GetLatestDeployment("non-existent-app", domain.EnvProd)
	if err != nil {
		t.Fatalf("GetLatestDeployment failed: %v", err)
	}

	if latest != nil {
		t.Error("Expected nil for non-existent app")
	}
}
