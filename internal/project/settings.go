package project

import gdproject "github.com/bon5co/godjango/project"

type Settings struct{}

func (Settings) Validate() error { return nil }

func Configure() (*gdproject.Project, error) {
	return gdproject.New(Settings{}, Apps()...)
}
