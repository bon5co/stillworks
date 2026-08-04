# Deploying stillworks

The stack is `dokploy-compose.yml`, run on calico as a raw compose stack against
a prebuilt image. Everything the running instance needs comes in through the
environment.

## Required

| Variable            | What it is                                                     |
| ------------------- | -------------------------------------------------------------- |
| `POSTGRES_PASSWORD` | Password for the bundled `stillworks-db` service.               |
| `SESSION_SECRET`    | Session and CSRF key material. Any long random string.          |
| `PUBLIC_ORIGIN`     | `https://stillworks.supercapybara.com` — exactly scheme://host. |

### `PUBLIC_ORIGIN`

The address this deployment is reached under. It is what the canonical link and
the social card's `og:image` are resolved against, and both are absolute by
definition.

**No trailing slash, no path, no query.** The value must be exactly
`scheme://host[:port]`. Anything else is refused at startup and the process
exits with the reason — deliberately, because the alternative failure is silent
and total: the head is resolved before a byte of the page is written, so a
malformed origin would make every HTML page answer `200` with an empty body
while `/healthz` stayed `204` and `/api/llm/up` kept serving JSON. Healthy by
every automated measure, blank for every human.

Leaving it unset is safe and is not a degraded deployment in the way a missing
database URL would be: the site serves normally, keeps its description and its
tab icon, and simply publishes no canonical URL and no link-preview card. It
does **not** fall back to the request's `Host` header — that is the client's own
text, and resolving a canonical URL against it is how a request arriving under
somebody else's name gets that name written into this site's canonical URL.

So: set it, and get the social card working. Forget it, and lose only the card.
Typo it, and the process refuses to start rather than serving nothing quietly.

### `TRUST_PROXY_HEADERS`

Optional, defaults to `false`, and **should be left unset on this stack.**

It tells the framework to believe `X-Forwarded-For` and `X-Forwarded-Proto`,
which is what `web.RemoteIP` and the CSRF origin check read. Turning it on is
only safe where nothing but the proxy can open a connection to the port. On
Dokploy the app sits on `dokploy-network`, which is shared with every other
stack on the host, so that guarantee does not hold: any container on that
network can dial this one directly and name its own address and scheme.

The one thing it would buy — an `https` scheme in the document head behind the
TLS-terminating proxy — `PUBLIC_ORIGIN` buys outright and without trusting
anybody. Nothing in the application reads `web.RemoteIP` today.

Note that the traffic counts and the per-visitor test-call limit are keyed on
`audit.clientIP`, which reads `CF-Connecting-IP` and friends directly and is not
governed by this setting either way. Making those read the framework's resolved
address is a separate change with its own effect on the published numbers.

### `PLAUSIBLE_HOST`

Optional, defaults to unset. The Plausible instance this deployment reports to —
`https://plausible.supercapybara.com` on the live stack. Exactly
`scheme://host[:port]`, same shape and same startup refusal as `PUBLIC_ORIGIN`.

Unset means no analytics at all: the pages link no analytics script, no route
forwards anything, and the site's own traffic recorder carries on alone. That is
what local development and the test suite run.

Set, it additionally **requires `PUBLIC_ORIGIN`** and the process refuses to
start without it. Plausible files an event under the domain the event names, and
one naming nothing is answered `202` and then discarded — a deployment that
looked configured and reported forever zero.

Everything Plausible needs is served from this origin:

| Path              | What it is                                                      |
| ----------------- | ---------------------------------------------------------------- |
| `/js/s.<hash>.js` | A loader that injects the tracking script with the site name.    |
| `/js/p.<hash>.js` | The vendored Plausible script, `apps/audit/static/plausible.js`. |
| `POST /api/event` | Relays one event to `PLAUSIBLE_HOST` from this server.           |

This is not a preference. The page's Content-Security-Policy is
`default-src 'self'`, so a `<script src="https://plausible.…/js/script.js">` is
dropped by the browser before it runs: no error a visitor sees, none in our
logs, no pageviews, and a page that looks exactly right. Serving from this
origin keeps the policy untouched and, as a bonus, survives the blocklists that
match on the analytics vendor's hostname.

Three things about that relay are worth knowing before changing it:

- **The visitor's address travels in `X-Plausible-IP`.** Plausible reads
  `X-Plausible-IP`, then `CF-Connecting-IP`, then `X-Forwarded-For`. Every
  request out of here crosses Cloudflare — which overwrites `CF-Connecting-IP`
  with *this server's* address — and then a Traefik that rewrites
  `X-Forwarded-For` to whatever it sees. Send only `X-Forwarded-For` and the
  dashboard reports one visitor, us, for the entire internet. Measured on
  2026-08-04 against a throwaway site: `X-Forwarded-For: 8.8.8.8` was filed
  under Japan; `X-Plausible-IP: 8.8.8.8` was filed under the United States.
- **Requests from `INTERNAL_NETWORKS` are dropped, not relayed.** The traffic
  recorder excludes our own deploy checks for a reason written on its own type;
  a second measurement repeating that mistake would be worse than no second
  measurement. To see a pageview of your own land in the dashboard, take your
  address out of `INTERNAL_NETWORKS` for as long as the check takes.
- **The routes live on the outermost mux, outside the application router.**
  `POST /api/event` carries no CSRF token, and the router's CSRF middleware
  compares the `Origin` header against the scheme *this process* sees — http
  behind the TLS-terminating proxy, https in the browser. Inside the router the
  event would be answered `403` in production and pass in local development.

Refreshing the vendored script: fetch `<PLAUSIBLE_HOST>/js/script.js` and
replace the body of `apps/audit/static/plausible.js`, keeping the provenance
comment at the top. Nothing in this repository generates that file. The
`data-domain` / `data-api` snippet is the documented alternative to Plausible's
newer per-site `pa-*.js` script, which needs an inline `<script>` this CSP
forbids.

## Optional: the key-required shelf

`/?key=key` publishes providers whose free tier needs an API key. We hold one
free-tier key per provider and probe with it. The keys live only in the
environment — never in the database, never in the repository, never in a
rendered page or an API response.

(This used to be its own page at `/llm/keyed/`. Since 0.9.0 there is one shelf
at `/`, with a key chip on every row and a filter in the URL; the old address
answers `301` to `/?key=key`.)

| Variable             | Provider   | Where the key comes from                                     |
| -------------------- | ---------- | ------------------------------------------------------------ |
| `GROQ_API_KEY`       | Groq       | <https://console.groq.com/keys>                              |
| `MISTRAL_API_KEY`    | Mistral AI | <https://admin.mistral.ai/> → workspace → API keys           |
| `OPENROUTER_API_KEY` | OpenRouter | <https://openrouter.ai/settings/keys>                        |

**Every one of these is optional and independent.** A provider whose variable is
unset is not probed at all. That is deliberate and is not a degraded state: its
models keep a null verdict, which the shelf renders as "not yet checked". The
alternative — calling it anyway and recording the 401 — would publish an outage
against somebody else's service when the only thing actually missing is a
variable on our side.

An instance with none of them set serves the keyless rows exactly as it did
before the second shelf existed, and `/?key=key` says nothing is verified.

The actual key values for this deployment are in the operator's key store, not
here. To find which variable an endpoint reads, look at `key_env` on its row in
`llm_endpoints`, or at `apps/audit/seed.go`.

### Rotating a key

Replace the variable's value and redeploy. Nothing else holds it: the prober
reads the environment on every request it builds, so there is no cached copy and
no restart-order problem. Probe history written under the old key stays valid —
it records what happened at the time it happened, which is the whole point.

### Adding a fourth provider

1. Get a free-tier key without a payment card and without a phone number. If a
   provider demands either, it does not go on this shelf.
2. Add a seed row in `apps/audit/seed.go` with `AuthMode: AuthModeKey` and
   `KeyEnv` naming a new `<PROVIDER>_API_KEY` variable.
3. Set `ChatProbes` if the provider publishes a request limit that three probes
   an hour would cross — OpenRouter's free tier allows 50 requests a day, so its
   row asks for one per cycle.
4. Add the variable to `dokploy-compose.yml` and to the table above.

The provider must speak `Authorization: Bearer <key>` on an OpenAI-compatible
surface. A provider that wants its credential somewhere else needs a new auth
mode in the migration and in `Prober.authorization`, not a config field.

## Probe schedule

| Variable              | Default | What it changes                                    |
| --------------------- | ------- | -------------------------------------------------- |
| `PROBE_INTERVAL`      | `1h`    | Liveness cycle: one model listing plus a few chat calls per endpoint. |
| `CAPABILITY_INTERVAL` | `24h`   | Feature cycle: tools, structured output, vision, image generation. |

Both accept a Go duration (`30m`, `2h`). Anything unparseable falls back to the
default rather than switching probing off, because an instance that has quietly
stopped checking is the exact failure this project exists to expose.

Per-endpoint probe budgets are a column, not an environment variable: the limit
being respected belongs to the provider, so it lives beside the note explaining
it in `seed.go`.
