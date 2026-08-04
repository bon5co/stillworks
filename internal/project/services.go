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
	// InternalNetworks lists the addresses whose requests are our own work, as
	// a comma-separated set of addresses or CIDR blocks. Empty means every
	// request counts as a visitor, which is how the first day reported fifteen
	// of them and meant none.
	InternalNetworks string
	// TrustProxyHeaders says whether the framework believes X-Forwarded-For and
	// X-Forwarded-Proto. It governs web.RemoteIP and web.RequestScheme, which is
	// what the framework's CSRF origin check and login redirect read.
	//
	// It does not currently govern who this application thinks a visitor is.
	// audit.clientIP reads CF-Connecting-IP, X-Forwarded-For and X-Real-IP
	// directly and unconditionally -- it predates this setting, and the traffic
	// counts and the per-visitor test-call limit are keyed on it. Turning this on
	// neither protects nor exposes those; they are spoofable either way, and
	// making them read web.RemoteIP is a separate change with its own effect on
	// the published numbers.
	//
	// Off by default, and it should stay off on any port something other than
	// the proxy can open a connection to. TrustAnyPeer's guarantee is
	// topological, and this deployment shares a Docker network with unrelated
	// stacks, so the guarantee does not hold there. The one thing it would buy --
	// an https scheme in the document head behind a TLS-terminating proxy --
	// PublicOrigin buys outright and without trusting anybody.
	TrustProxyHeaders bool
	// PublicOrigin is the scheme and host this deployment is reached under, for
	// example https://stillworks.supercapybara.com. Exactly scheme://host[:port]:
	// a path, a query or a trailing slash is refused at startup rather than
	// turning every page into an empty 200.
	//
	// It is what canonical and og:image URLs resolve against. Left empty, this
	// site publishes neither rather than resolving them against the Host header,
	// which is the client's own text.
	PublicOrigin string
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
		env.Optional("INTERNAL_NETWORKS", &settings.InternalNetworks, ""),
		env.Optional("TRUST_PROXY_HEADERS", &settings.TrustProxyHeaders, false),
		env.Optional("PUBLIC_ORIGIN", &settings.PublicOrigin, ""),
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
