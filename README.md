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
| `GROQ_API_KEY`, `MISTRAL_API_KEY`, `OPENROUTER_API_KEY` | Optional. Used only to probe the key-required shelf. |

Provider keys live in the environment and go out on probes only. They are never
rendered, logged, written to the database, or returned by the API — there is a test that
fails if one appears in a response.

## Adding an endpoint

Open a PR against `apps/audit/seed.go` with the base URL, the chat path, and whether it
needs a key. The prober will verify it on the next cycle and publish whatever actually
happens, including nothing.

MIT licensed.
