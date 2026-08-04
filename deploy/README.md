# Deploying stillworks

The stack is `dokploy-compose.yml`, run on calico as a raw compose stack against
a prebuilt image. Everything the running instance needs comes in through the
environment.

## Required

| Variable            | What it is                                                     |
| ------------------- | -------------------------------------------------------------- |
| `POSTGRES_PASSWORD` | Password for the bundled `stillworks-db` service.               |
| `SESSION_SECRET`    | Session and CSRF key material. Any long random string.          |

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
