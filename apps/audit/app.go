package audit

import (
	"embed"
	"io/fs"
)

type App struct{}

func New() *App { return &App{} }

func (*App) Name() string { return "audit" }

//go:embed migrations
var migrationFiles embed.FS

func (*App) MigrationFS() fs.FS {
	files, err := fs.Sub(migrationFiles, "migrations")
	if err != nil {
		panic(err)
	}
	return files
}
