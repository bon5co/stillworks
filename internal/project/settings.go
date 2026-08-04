package project

import (
	gdproject "github.com/bon5co/godjango/project"
	"stillworks/apps/audit"
)

type Settings struct{}

func (Settings) Validate() error { return nil }

func Configure() (*gdproject.Project, error) {
	return gdproject.New(Settings{}, Apps()...)
}

// ConfigureWith builds the project around an audit app the caller has already
// prepared. The server uses it to hand the app a traffic recorder.
func ConfigureWith(auditApp *audit.App) (*gdproject.Project, error) {
	return gdproject.New(Settings{}, AppsWith(auditApp)...)
}
