package project

import (
	"github.com/bon5co/godjango/auth"
	gdproject "github.com/bon5co/godjango/project"
	"stillworks/apps/audit"
)

func Apps() []gdproject.App {
	return []gdproject.App{
		auth.App,
		audit.New(),
	}
}
