package project

import (
	"context"
	"errors"
	"fmt"

	"github.com/bon5co/godjango/auth"
	"github.com/bon5co/godjango/database"
	"github.com/bon5co/godjango/env"
	"github.com/bon5co/godjango/management"
	"github.com/bon5co/godjango/migrations"
)

type DatabaseSettings struct {
	DatabaseURL env.Secret
}

type RuntimeSettings struct {
	DatabaseURL   env.Secret
	SessionSecret env.Secret
	Debug         bool
	Port          int
}

func LoadDatabaseSettings() (DatabaseSettings, error) {
	root, err := management.DiscoverProject(".")
	if err != nil {
		return DatabaseSettings{}, err
	}
	var settings DatabaseSettings
	schema := env.New(
		env.Required("DATABASE_URL", &settings.DatabaseURL),
	)
	if err := schema.Load(env.WithWorkingDirectory(root)); err != nil {
		return DatabaseSettings{}, err
	}
	return settings, nil
}

func LoadRuntimeSettings() (RuntimeSettings, error) {
	root, err := management.DiscoverProject(".")
	if err != nil {
		return RuntimeSettings{}, err
	}
	var settings RuntimeSettings
	schema := env.New(
		env.Required("DATABASE_URL", &settings.DatabaseURL),
		env.Required("SESSION_SECRET", &settings.SessionSecret),
		env.Optional("DEBUG", &settings.Debug, false),
		env.Optional("PORT", &settings.Port, 8000),
	)
	if err := schema.Load(env.WithWorkingDirectory(root)); err != nil {
		return RuntimeSettings{}, err
	}
	return settings, nil
}

func Services() management.ProjectServices {
	return management.ProjectServices{
		Migrations: openMigrations,
		Users:      openUsers,
		Database: func(ctx context.Context) (*database.DB, func() error, error) {
			db, err := openDatabase(ctx)
			if err != nil {
				return nil, nil, err
			}
			return db, db.Close, nil
		},
		RunServer: func(
			ctx context.Context,
			args []string,
			streams management.Streams,
		) error {
			settings, err := LoadRuntimeSettings()
			if err != nil {
				return err
			}
			if len(args) == 0 {
				args = []string{fmt.Sprintf("127.0.0.1:%d", settings.Port)}
			}
			root, err := management.DiscoverProject(".")
			if err != nil {
				return err
			}
			return management.RunProjectProgram(ctx, root, "./cmd/server", args, streams)
		},
		DatabaseShell: func(
			ctx context.Context,
			args []string,
			streams management.Streams,
		) error {
			settings, err := LoadDatabaseSettings()
			if err != nil {
				return err
			}
			return management.RunDatabaseShell(ctx, settings.DatabaseURL.Reveal(), args, streams)
		},
	}
}

func openDatabase(ctx context.Context) (*database.DB, error) {
	settings, err := LoadDatabaseSettings()
	if err != nil {
		return nil, err
	}
	return database.Open(ctx, database.DefaultConfig(settings.DatabaseURL.Reveal()))
}

func openMigrations(
	ctx context.Context,
) (management.MigrationManager, func() error, error) {
	db, err := openDatabase(ctx)
	if err != nil {
		return nil, nil, err
	}
	configured, err := Configure()
	if err != nil {
		return nil, nil, errors.Join(err, db.Close())
	}
	catalog, err := migrations.Collect(configured)
	if err != nil {
		return nil, nil, errors.Join(err, db.Close())
	}
	runner, err := migrations.NewRunner(db, catalog, migrations.DefaultRunnerConfig())
	if err != nil {
		return nil, nil, errors.Join(err, db.Close())
	}
	return runner, db.Close, nil
}

func openUsers(
	ctx context.Context,
) (management.UserManager, func() error, error) {
	db, err := openDatabase(ctx)
	if err != nil {
		return nil, nil, err
	}
	manager := auth.NewManager(auth.NewBunStore(db), auth.NewPasswordHasher())
	return manager, db.Close, nil
}
