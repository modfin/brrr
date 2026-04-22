package brrr_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/modfin/brrr"
)

var testContainer *brrr.Container

func TestMain(m *testing.M) {
	c, err := brrr.NewContainer(brrr.Config{
		User:     "postgres",
		Password: "postgres",
		Database: "brrr_test",
	})
	if err != nil {
		panic("failed to start test container: " + err.Error())
	}
	testContainer = c
	code := m.Run()
	if err := c.Close(); err != nil {
		panic("failed to close test container: " + err.Error())
	}
	os.Exit(code)
}

// TestNewContainer_StartsCleanly is the regression test for the wait.ForSQL URL
// construction bug introduced when testcontainers-go migrated to moby modules in
// v0.42. The callback was receiving the port as "<num>/<proto>" (e.g. "5432/tcp"),
// and embedding the suffix into the URL path, which corrupted the dbname and made
// the SQL wait strategy fail until the 10s startup timeout expired.
// If TestMain completes successfully, this test passes; it exists to name the
// guarantee explicitly so future regressions surface as a named failure.
func TestNewContainer_StartsCleanly(t *testing.T) {
	if testContainer == nil {
		t.Fatal("test container was not initialized in TestMain")
	}
}

func TestContainer_NewInstance_IsUsable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	di, err := testContainer.NewInstance(ctx)
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	t.Cleanup(func() {
		if err := testContainer.CloseInstance(context.Background(), di); err != nil {
			t.Errorf("CloseInstance: %v", err)
		}
	})

	var got int
	if err := di.Connection.QueryRow(ctx, "SELECT 1").Scan(&got); err != nil {
		t.Fatalf("SELECT 1 against instance failed: %v", err)
	}
	if got != 1 {
		t.Fatalf("expected 1, got %d", got)
	}
}

func TestContainer_NewInstance_InstancesAreIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	a, err := testContainer.NewInstance(ctx)
	if err != nil {
		t.Fatalf("NewInstance a: %v", err)
	}
	t.Cleanup(func() { _ = testContainer.CloseInstance(context.Background(), a) })

	b, err := testContainer.NewInstance(ctx)
	if err != nil {
		t.Fatalf("NewInstance b: %v", err)
	}
	t.Cleanup(func() { _ = testContainer.CloseInstance(context.Background(), b) })

	if a.Name == b.Name {
		t.Fatalf("expected distinct instance names, both were %q", a.Name)
	}

	if _, err := a.Connection.Exec(ctx, "CREATE TABLE isolation_probe (v int)"); err != nil {
		t.Fatalf("create table on a: %v", err)
	}
	if _, err := a.Connection.Exec(ctx, "INSERT INTO isolation_probe (v) VALUES (42)"); err != nil {
		t.Fatalf("insert on a: %v", err)
	}

	var exists bool
	err = b.Connection.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'isolation_probe')").Scan(&exists)
	if err != nil {
		t.Fatalf("lookup on b: %v", err)
	}
	if exists {
		t.Fatal("expected isolation_probe to not exist on instance b; template contamination")
	}
}
