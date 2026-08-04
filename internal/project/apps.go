package project

import (
	"github.com/bon5co/godjango/auth"
	gdproject "github.com/bon5co/godjango/project"
	"stillworks/apps/audit"
)

func Apps() []gdproject.App {
	return AppsWith(audit.New())
}

// AppsWith lets the server process supply an audit app it has already wired --
// with the traffic recorder, whose lifetime belongs to main. A management
// command uses the plain Apps() above: it collects migrations and never serves
// a request, so it has nothing to count.
func AppsWith(auditApp *audit.App) []gdproject.App {
	return []gdproject.App{
		auth.App,
		auditApp,
	}
}
