# stillworks

**Directories list. We check.**

A live-probed registry of LLM endpoints. Every row on
[stillworks.supercapybara.com](https://stillworks.supercapybara.com) was produced by a
real chat completion sent on a schedule — not by reading a provider's documentation —
and the failures are published next to the successes.

```bash
curl -s https://stillworks.supercapybara.com/api/llm/up
```

Returns the endpoints that answered **with no Authorization header at all**, ranked by
how much of the last week each one answered and then by latency. Each entry carries
everything the next call needs:

```json
{
  "model": "minimax-m2.7",
  "provider": "llm7",
  "openai_base_url": "https://api.llm7.io/v1",
  "auth": "none",
  "latency_ms": 1446,
  "answered": "10/10",
  "last_ok": "2026-08-04T12:34:45Z",
  "proved": ["tools", "json_schema", "json_object"],
  "claimed_unproved": [],
  "failed": ["vision"]
}
```

Because the array is ranked, `models[0]` is the pick and the rest are your fallbacks —
which you will want. Keyless quotas are commonly per-IP, so an endpoint that answers our
server can still refuse yours. Walking the list is the intended usage, not a workaround:

```python
import json, urllib.request
from openai import OpenAI

shelf = json.load(urllib.request.urlopen(
    "https://stillworks.supercapybara.com/api/llm/up"))

for endpoint in shelf["models"]:            # already ranked, best first
    client = OpenAI(base_url=endpoint["openai_base_url"], api_key="not-needed")
    try:
        reply = client.chat.completions.create(
            model=endpoint["model"],
            messages=[{"role": "user", "content": "hello"}],
        )
    except Exception:
        continue                            # your IP hit its quota — next
    print(endpoint["model"], reply.choices[0].message.content)
    break
```

## How search engines and agents find it

Every address that is content is listed once, in `indexablePages()` in
`apps/audit/pages.go`, plus one page per active endpoint from the database. That
one list builds `/sitemap.xml` (with a day-precision `lastmod`), `/llms.txt`, and
is what the test suite holds the router against: a page cannot be served and
missing from the sitemap, or listed and not served.

| Address | Indexed | What it is |
| --- | --- | --- |
| `/` | yes | The shelf. Title, h1 and meta lead with "free LLM API". |
| `/free-llm-api-no-key` | yes | Keyless providers with their 14-day track record. |
| `/free-llm-api-resources-alternatives` | yes | What a static list cannot tell you, and what an hourly probe adds. |
| `/docs` | yes | The `/api/llm/up` reference, with a live example row. |
| `/sponsor` | yes | The sponsored slot: price, what it is and is not, audience figures read live. `/go/sponsor` (the tracked redirect) and `/api/sponsor` are `noindex`. |
| `/llm/<slug>` | yes | One page per endpoint: a track-record section computed from the probe table, then the models and the raw probe log. |
| `/stats`, `/mcp/`, 404 and error pages | no | `X-Robots-Tag: noindex, follow`. |
| `/api/*` | no | `X-Robots-Tag: noindex`. A header, not a `Disallow`: a crawler forbidden to fetch a URL never reads it. |
| `/llm/`, `/llm/keyed/`, `/llm/try` | no | `301` to the shelf. |

`robots.txt` allows everything, disallows only `/api/llm/try` (which makes an
outbound call), and carries the `Sitemap:` line. AI crawlers are not blocked:
the audience is developers and the agents they run.

Titles are held to 60 characters and descriptions to 155 by a test. Each template
has its own 1200x630 card in `apps/audit/static/card-*.png`, served from this
origin because the CSP is `default-src 'self'`. JSON-LD is written into the body:
a `type="application/ld+json"` block is data, so the CSP does not touch it.

When you add a page that is content, add its `pageDef` and its entry in
`indexablePages()`. When you add an endpoint, its page and sitemap entry follow
from the database, and its title falls back to `providerHead`'s template unless
you give it a line there.

## The sponsored field

`/api/llm/up` also carries one top-level `sponsored` field: `null` when no sponsor holds
the slot, an object when one does. It is paid placement and says so.

```json
"sponsored": {
  "sponsored": true,
  "label": "Sponsored",
  "notice": "Paid placement. Not probed, not ranked, and not part of the models array: ...",
  "provider": "Acme Inference",
  "pitch": "Free-tier Llama and Qwen, no card.",
  "url": "https://stillworks.supercapybara.com/go/sponsor?src=api",
  "destination_host": "acme.example",
  "model": "acme-8b",
  "base_url": "https://api.acme.example/v1",
  "starts_on": "2026-10-01",
  "ends_on": "2026-10-31",
  "probe": null
}
```

- **It is never in `models` and never changes the ranking.** `models[0]` is still the pick and
  the rest is still the fallback chain. A test holds the `models` array byte-identical with and
  without a sponsor.
- `probe` is the real probe row for the sponsor's endpoint when that endpoint is on the shelf,
  shaped exactly like a `models` entry, and `null` when it is not. It is never synthesised.
- `url` goes through our own `/go/sponsor` redirect so clicks can be counted; the destination
  host is published beside it. `model` and `base_url` appear only when the sponsor supplied them.
- Programs that only want the ranked list can ignore the field; nothing else in the response moved.

The slot is sold on [/sponsor](https://stillworks.supercapybara.com/sponsor). The audience figures
printed there (API calls, distinct API clients per day, Plausible-measured human visitors) are
also at `GET /api/sponsor`.

## What "proved" means

A feature is listed as proved only when a real call demonstrated it:

| Field | Meaning |
| --- | --- |
| `proved` | We called it and it worked — a tool call we could dispatch, a reply that parsed and matched the schema we sent, an answer about an image we sent. |
| `claimed_unproved` | The provider claims the feature, and our call has never demonstrated it. |
| `failed` | We called it and it failed. |
| absent from all three | Never probed. Not the same as failing. |

A capability filter (`?feature=tools,vision`) returns only models where a real call
proved the feature, so unprobed models are excluded rather than optimistically included.

## Query parameters

| Parameter | Values | Default |
| --- | --- | --- |
| `key` | `none`, `key`, `any` | `none` |
| `feature` | `tools`, `json_schema`, `json_object`, `vision` (comma-separated) | — |
| `q` | substring match on model, provider, or base URL | — |

The same parameters drive the HTML page, so a filtered shelf is a link you can send.

## What this cannot tell you

- **Keyless quotas are usually per-IP.** Verified from our address is not verified from
  yours. This is stated on every page rather than buried, because it is the single most
  common way a list like this misleads.
- Rows marked `auth: "bearer"` were probed with **our own** free-tier key. That proves the
  endpoint answered our account. It says nothing about what your signup's free tier
  includes or whether the provider is still handing out accounts.
- These are other people's free services. Any of them can add a key requirement or
  disappear between two probes.

## Running it

Go, [godjango](https://github.com/bon5co/godjango), Postgres, templ. No JavaScript
framework; the page filters and sorts server-side and the client script only enhances.

```bash
docker compose up --build      # http://localhost:8080
go test ./...
```

Configuration is environment-only:

| Variable | Purpose |
| --- | --- |
| `PUBLIC_ORIGIN` | `scheme://host`, no trailing slash. Absolute URLs for social cards. |
| `INTERNAL_NETWORKS` | Addresses excluded from the traffic counters, so our own checks are not counted as visitors. |
| `PLAUSIBLE_HOST` | Optional. Analytics events are relayed server-side. |
| `PLAUSIBLE_STATS_KEY` | Optional. A Plausible Stats API key, so `/sponsor` can print the 30-day human-visitor figure. Without it the figure shows as unavailable. |
| `SPONSOR_CHECKOUT_URL` | Optional. Hosted checkout link for the monthly slot. Without it `/sponsor` offers email. |
| `GROQ_API_KEY`, `MISTRAL_API_KEY`, `OPENROUTER_API_KEY` | Optional. Used only to probe the key-required shelf. |

Provider keys live in the environment and go out on probes only. They are never
rendered, logged, written to the database, or returned by the API — there is a test that
fails if one appears in a response.

## What was checked and left off

The shelf is short, and a short list is easy to mistake for a lazy one. So the rejects
are published too, under the table and in `apps/audit/crosscheck.go`: what was called,
and what it answered.

Two sweeps so far, both against
[OmniRoute](https://github.com/diegosouzapw/OmniRoute) — a routing registry that types
every provider by auth mode, which makes "needs no key" a claim you can check by machine
instead of by reading marketing copy.

- **2026-08-11:** 226 providers, 18 typed keyless or optional. Calling all 18 from this
  server produced one new claim on the shelf and no new working keyless endpoint.
- **2026-10-12:** 270 providers (58 new directories, 11 gone), 17 typed keyless or
  optional. Also called: the two that ship a public placeholder key, the thirteen the
  registry's free-tier list tags keyless, and one anonymous chat call to each of the 52
  entries added since August. Result: **two** new working keyless endpoints, Kilo Gateway
  and Free.ai, now on the shelf. Everything else failed the chat call and is listed under
  the table with what it answered. Forty-two further providers publish a recurring free tier
  but need a signup; they are not listed here because the key-required shelf only carries
  providers we hold a key for.

The recurring shape is worth knowing if you maintain a list like this: several of them
answer `GET /v1/models` with a full catalogue and no credential, then answer the chat
call with `401`. A keyless listing is not a keyless endpoint, and only the second call
tells you which you have.

## The sponsored slot

One paid placement: a labelled card above the shelf and a `sponsored` field in
`/api/llm/up`. It lives in the `sponsors` table, so it changes without a deploy:

```bash
manage sponsor set -provider "Acme Inference" -pitch "Free-tier Llama, no card." \
  -url "https://acme.example/signup?ref=stillworks" -from 2026-11-01 -to 2026-11-30 \
  [-model acme-8b] [-base-url https://api.acme.example/v1] [-endpoint <slug on the shelf>]
manage sponsor list      # ACTIVE / scheduled / ended
manage sponsor end       # end whoever holds it today
manage sponsor report    # API calls, client-days, clicks from the page vs from the API
```

The database refuses two sponsors whose dates overlap. With none active the card is a house
ad that links to `/sponsor`. Clicks go through `/go/sponsor` and are stored in `traffic_events`
as kind `sponsor`, with the source (`page` or `api`) in the path; crawlers and our own
requests are excluded from every figure.

## Adding an endpoint

Open a PR against `apps/audit/seed.go` with the base URL, the chat path, and whether it
needs a key. The prober will verify it on the next cycle and publish whatever actually
happens, including nothing.

MIT licensed.
