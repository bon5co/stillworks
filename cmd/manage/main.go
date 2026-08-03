package main

import (
	"context"
	"fmt"
	"os"

	"github.com/bon5co/godjango/management"
	configuredproject "stillworks/internal/project"
)

func main() {
	configured, err := configuredproject.Configure()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(management.ExitFailure)
	}
	os.Exit(management.ExecuteProject(
		context.Background(),
		os.Args[1:],
		management.ProjectOptions{
			Project:  configured,
			Services: configuredproject.Services(),
			Commands: configuredproject.Commands(),
		},
		management.Streams{In: os.Stdin, Out: os.Stdout, Err: os.Stderr},
	))
}
