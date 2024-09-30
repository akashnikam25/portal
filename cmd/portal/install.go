package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jmoiron/sqlx"
	"github.com/knadh/koanf/v2"
)

func installSchema(ver string, app *App, prompt bool, ko *koanf.Koanf) {
	if prompt {
		fmt.Println("")
		fmt.Println("** first time installation **")
		fmt.Printf("** IMPORTANT: This will wipe existing tables and types in the DB '%s' **",
			ko.String("db.db"))
		fmt.Println("")

		if prompt {
			var ok string
			fmt.Print("continue (y/n)?  ")
			if _, err := fmt.Scanf("%s", &ok); err != nil {
				fmt.Printf("error reading value from terminal: %v", err)
				os.Exit(1)
			}
			if strings.ToLower(ok) != "y" {
				fmt.Println("install cancelled.")
				return
			}
		}
	}

	q, err := app.fs.Read("/schema.sql")
	if err != nil {
		app.lo.Fatal(err.Error())
		return
	}

	if _, err := app.db.Exec(string(q)); err != nil {
		app.lo.Fatal(err.Error())
		return
	}

	// Insert the current migration version.
	if err := recordMigrationVersion(ver, app.db); err != nil {
		app.lo.Fatal(err)
	}

	app.lo.Println("successfully installed Postgres schema")

	// Install Typesense schema.
	app.lo.Println("installing Typesense schema")

	s := initSearch(ko)
	if err := s.InitSchema(); err != nil {
		app.lo.Fatal(err)
	}

	app.lo.Println("done")
}

// recordMigrationVersion inserts the given version (of DB migration) into the
// `migrations` array in the settings table.
func recordMigrationVersion(ver string, db *sqlx.DB) error {
	_, err := db.Exec(fmt.Sprintf(`INSERT INTO settings (key, value)
	VALUES('migrations', '["%s"]'::JSONB)
	ON CONFLICT (key) DO UPDATE SET value = settings.value || EXCLUDED.value`, ver))
	return err
}
