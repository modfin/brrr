# BRRR
Brrr is an integration testing thingy for go applications using postgres.

## What does it do and what does it try to solve
1. Launches a single go test container for postgres.
2. Optionally runs seeding migration scripts.
3. Marks the database as a template database.
4. For each tests, creates a new database from the template so each test can have its own database in isolation.
5. Runs isolated integration tests in parallell, so you can make some brrr noises while waiting.

## Why The name
It goes fast. No benchmarks because simple benchmarks are stupid.

## Example
```
var c *brrr.Container

func TestMain(m *testing.M) {
	cfg := brrr.Config {
		User: "postgres",
		Password: "test",
		Database: "acme",
	}

	var err error
	c, err = brrr.NewContainer(cfg)
	if err != nil {
		log.Fatalf("failed to create test container: %v", err)
	}
	defer tc.Close()

	exitCode := m.Run()

	os.Exit(exitCode)
}

func TestSomething(t *testing.T) {
	t.Parallel()

	db, err := c.NewInstance(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseInstance(t.Context(), db)

	err = db.Connection.Ping(t.Context())
	if err != nil {
		t.Fatalf("failed to ping database: %v", err)
	}
}
```
