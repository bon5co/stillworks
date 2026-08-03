package audit

import "github.com/go-chi/chi/v5"

func (*App) Routes(router chi.Router) {
	RegisterRoutes(router)
}

func RegisterRoutes(chi.Router) {}
