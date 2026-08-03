package project

import (
	"github.com/bon5co/godjango/management"
	"stillworks/apps/audit"
)

func Commands() []management.Command {
	services := Services()
	var registered []management.Command
	registered = append(registered, audit.Commands(services)...)
	return registered
}
