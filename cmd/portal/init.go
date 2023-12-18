package main

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io/ioutil"
	mrand "math/rand"
	"os"
	"unicode"

	"floss.fund/portal/internal/core"
	"floss.fund/portal/internal/crawl"
	v1 "floss.fund/portal/internal/schemas/v1"
	"github.com/jmoiron/sqlx"
	"github.com/knadh/goyesql/v2"
	goyesqlx "github.com/knadh/goyesql/v2/sqlx"
	"github.com/knadh/koanf/parsers/toml"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/knadh/koanf/v2"
	"github.com/knadh/stuffbin"
	"github.com/labstack/echo/v4"
	flag "github.com/spf13/pflag"
)

func initConfig() {
	// Commandline flags.
	f := flag.NewFlagSet("config", flag.ContinueOnError)

	f.Usage = func() {
		fmt.Println(f.FlagUsages())
		fmt.Printf("floss.fund portal (%s) tool", versionString)
		os.Exit(0)
	}

	f.String("mode", "site", "site = runs the public portal | crawl = runs the background crawler")
	f.Bool("new-config", false, "generate a new sample config.toml file.")
	f.StringSlice("config", []string{"config.toml"},
		"path to one or more config files (will be merged in order)")
	f.Bool("install", false, "run first time DB installation")
	f.Bool("upgrade", false, "upgrade database to the current version")
	f.Bool("yes", false, "assume 'yes' to prompts during --install/upgrade")
	f.Bool("version", false, "current version of the build")

	if err := f.Parse(os.Args[1:]); err != nil {
		lo.Fatalf("error parsing flags: %v", err)
	}

	if ok, _ := f.GetBool("version"); ok {
		fmt.Println(buildString)
		os.Exit(0)
	}

	// Generate new config file.
	if ok, _ := f.GetBool("new-config"); ok {
		if err := generateNewFiles(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("config.toml generated. Edit and run --install.")
		os.Exit(0)
	}

	// Load config files.
	cFiles, _ := f.GetStringSlice("config")
	for _, f := range cFiles {
		lo.Printf("reading config: %s", f)

		if err := ko.Load(file.Provider(f), toml.Parser()); err != nil {
			fmt.Printf("error reading config: %v", err)
			os.Exit(1)
		}
	}

	if err := ko.Load(posflag.Provider(f, ".", ko), nil); err != nil {
		lo.Fatalf("error loading config: %v", err)
	}
}

func initConstants(ko *koanf.Koanf) Consts {
	c := Consts{
		RootURL:      ko.MustString("app.root_url"),
		ManifestURI:  ko.MustString("app.manifest_uri"),
		WellKnownURI: ko.MustString("app.wellknown_uri"),
	}

	return c
}

// initDB initializes a database connection.
func initDB(host string, port int, user, pwd, dbName string) *sqlx.DB {
	db, err := sqlx.Connect("postgres",
		fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable", host, port, user, pwd, dbName))
	if err != nil {
		lo.Fatalf("error initializing DB: %v", err)
	}

	return db
}

// initFS initializes the stuffbin FileSystem to provide
// access to bunded static assets to the app.
func initFS() stuffbin.FileSystem {
	path, err := os.Executable()
	if err != nil {
		lo.Fatalf("error getting executable path: %v", err)
	}

	fs, err := stuffbin.UnStuff(path)
	if err == nil {
		return fs
	}

	// Running in local mode. Load the required static assets into
	// the in-memory stuffbin.FileSystem.
	lo.Printf("unable to initialize embedded filesystem: %v", err)
	lo.Printf("using local filesystem for static assets")

	files := []string{
		"config.sample.toml",
		"queries.sql",
		"schema.sql",
	}

	fs, err = stuffbin.NewLocalFS("/", files...)
	if err != nil {
		lo.Fatalf("failed to load local static files: %v", err)
	}

	return fs
}

func initHTTPServer(app *App, ko *koanf.Koanf) *echo.Echo {
	srv := echo.New()
	srv.Debug = true
	srv.HideBanner = true

	// Register app (*App) to be injected into all HTTP handlers.
	srv.Use(func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c echo.Context) error {
			c.Set("app", app)
			return next(c)
		}
	})

	initHandlers(srv)

	return srv
}

func initCore(fs stuffbin.FileSystem, db *sqlx.DB, ko *koanf.Koanf) *core.Core {
	// Load SQL queries.
	qB, err := fs.Read("/queries.sql")
	if err != nil {
		lo.Fatalf("error reading queries.sql: %v", err)
	}

	qMap, err := goyesql.ParseBytes(qB)
	if err != nil {
		lo.Fatalf("error loading SQL queries: %v", err)
	}

	// Map queries to the query container.
	var q core.Queries
	if err := goyesqlx.ScanToStruct(&q, qMap, db.Unsafe()); err != nil {
		lo.Fatalf("no SQL queries loaded: %v", err)
	}

	opt := core.Opt{}

	return core.New(&q, opt)
}

func initCrawl(db *sqlx.DB, ko *koanf.Koanf) *crawl.Crawl {
	opt := crawl.Opt{
		UserAgent:    ko.MustString("crawl.useragent"),
		MaxHostConns: ko.MustInt("crawl.max_host_conns"),
		ReqTimeout:   ko.MustDuration("crawl.req_timeout"),
		Attempts:     ko.MustInt("crawl.attempts"),
		MaxBytes:     ko.MustInt64("crawl.max_bytes"),
	}

	sc := v1.New("v1.0.0", &v1.Opt{
		WellKnownPath: ko.MustString("app.wellknown_path"),
	})
	return crawl.New(opt, sc)
}

func generateNewFiles() error {
	if _, err := os.Stat("config.toml"); !os.IsNotExist(err) {
		return errors.New("config.toml exists. Remove it to generate a new one")
	}

	// Initialize the static file system into which all
	// required static assets (.sql, .js files etc.) are loaded.
	fs := initFS()

	// Generate config file.
	b, err := fs.Read("config.sample.toml")
	if err != nil {
		return fmt.Errorf("error reading sample config (is binary stuffed?): %v", err)
	}

	// Inject a random password.
	p := make([]byte, 12)
	rand.Read(p)
	pwd := []byte(fmt.Sprintf("%x", p))

	for i, c := range pwd {
		if mrand.Intn(4) == 1 {
			pwd[i] = byte(unicode.ToUpper(rune(c)))
		}
	}

	b = bytes.Replace(b, []byte("dictpress_admin_password"), pwd, -1)

	if err := ioutil.WriteFile("config.toml", b, 0644); err != nil {
		return err
	}

	return nil
}