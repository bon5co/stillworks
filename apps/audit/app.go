package audit

import (
	"embed"
	"io/fs"
)

type App struct {
	// recorder is nil for a management process, which wires the app to collect
	// migrations and never serves a request. Routes treat a nil recorder as
	// "count nothing" rather than refusing to start: losing the numbers is
	// recoverable, a site that will not boot is not.
	recorder *Recorder
}

func New() *App { return &App{} }

// UseRecorder hands the app the traffic recorder. The caller owns its
// lifetime -- it is the process's shutdown context that decides when the last
// batch is flushed -- so it is passed in rather than created here.
func (a *App) UseRecorder(recorder *Recorder) { a.recorder = recorder }

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
