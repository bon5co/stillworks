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

## Optional: the key-required shelf

`/llm/keyed/` publishes providers whose free tier needs an API key. We hold one
free-tier key per provider and probe with it. The keys live only in the
environment — never in the database, never in the repository, never in a
rendered page or an API response.

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

An instance with none of them set serves the keyless shelf exactly as it did
before the second shelf existed, and `/llm/keyed/` says nothing is verified.

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
