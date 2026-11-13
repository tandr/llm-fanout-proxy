# llm-fanout-proxy

A small Go gateway for Codex: native Responses forwarding, on-demand model discovery,
and Responses-to-Chat-Completions translation for external providers. HTTP and protocol
handling use the standard library and `github.com/coder/websocket`; YAML parsing uses
`go.yaml.in/yaml/v3`. Each request selects one upstream; requests are never broadcast.

## Run

Requires Go 1.24 or newer.

```sh
cp examples/fanout.yaml ./fanout.yaml
export FANOUT_API_KEY='a-separate-local-gateway-key'
export LITHOS_API_KEY='your-provider-key'
go run ./cmd/fanout -config ./fanout.yaml -check
go run ./cmd/fanout -config ./fanout.yaml
```

Keep credentials in the environment. Configuration contains environment variable names,
not keys. The file uses a small YAML schema; LiteLLM configuration is not loaded
directly. The listener defaults to loopback. `/health/readiness` reports readiness;
`/stats` reports aggregate history-cache counters in JSON.

The gateway accepts the existing Codex paths under `/backend-api/codex` and the
`x-litellm-api-key` header for local authentication. Native requests retain their
ChatGPT authentication and account headers; external requests use only their
configured provider credentials. The gateway key is removed before native forwarding.

## Routes and discovery

Unprefixed native model names go to the native upstream. `chatgpt/` is a native alias.
A configured prefix such as `lithos/` selects that provider; the prefix is removed from
the upstream model name for direct providers. Unknown external prefixes fail explicitly.

Provider protocols:

- `chat_completions`: translate Responses requests and output in Go.
- `responses`: forward to a native Responses endpoint.

Model catalogs are fetched on demand and merged with the native Codex catalog. Native
records and instructions are retained. `model_overrides` adds verified context windows
and reasoning levels without disabling discovery. The first matching pattern wins;
patterns match upstream IDs without the routing prefix. Put specific patterns first.

## Codex connection

Run the gateway as a separate process, then point a Codex provider at
`http://127.0.0.1:8765/backend-api/codex`, with `wire_api = "responses"`,
`supports_websockets = true`, and the gateway header supplied through the environment:

```toml
[model_providers.fanout]
name = "Local Go gateway"
base_url = "http://127.0.0.1:8765/backend-api/codex"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = true
env_http_headers = { "x-litellm-api-key" = "FANOUT_API_KEY" }
```

Select with `-c 'model_provider="fanout"'` for a new session. Existing sessions can be
bound to their original provider ID; replacing that provider's endpoint requires
an explicit configuration change. HTTP remains supported with `supports_websockets = false`.
Building this repository does not change the installed launcher, start a daemon,
or activate the gateway.

For the managed local launcher, install with
`go build -o ~/.local/bin/llm-fanout-proxy ./cmd/fanout` and put the YAML configuration
at `~/.config/fanout.yaml`. Set its `api_key_env` to `FANOUT_API_KEY`; the
launcher generates that local key for each launch. Provider credentials come from
the environment; the shell launcher also sources `~/.config/fanout.env` when present.
Use ordinary shell exports there, such as `export LITHOS_API_KEY='...'`, and keep
that file mode 0600.

The launcher starts Go directly with `-listen 127.0.0.1:PORT -external-only`, retaining
its existing readiness and process-lifetime checks. This mode discovers only the
configured external providers and rejects native OpenAI models and endpoints.
Select it with `codexx --fanout`; it has a separate `fanout` provider ID and
`fanout.sock` control socket. `codexx --litellm` keeps its existing LiteLLM route,
configuration, and runtime. Normal `codexx` and `codexx --openai` continue using the
native route. Existing profiles and running servers are unchanged.

The managed launcher enables WebSockets for the fanout route. After installing an
updated binary or changing transport settings, restart its server with
`codexx --fanout app-server daemon restart` and start a new session. The LiteLLM
route retains its existing HTTP transport.

## Translation boundaries

The Chat Completions adapter accepts full-history HTTP requests and incremental
HTTP/WebSocket requests using `previous_response_id`. It translates function tools,
namespaced tools, freeform custom tools, text, reasoning, usage, and streaming output. Custom-tool grammars are provided to the model
as instructions; they are not enforced by the gateway.

Opaque compaction, encrypted-only reasoning, and unsupported hosted tools fail
explicitly. Native Responses traffic is forwarded without applying
those translation restrictions. Model capabilities must reflect the selected provider;
discovering a model ID alone does not establish vision or hosted-tool support.

Client cancellation cancels upstream work. WebSocket `response.create` supports Codex
prewarm (`generate: false`, no model request) and `response.interrupt` with
`mode: "discard_partial_items"`. Interruption returns an acknowledgment and an
`interrupted` incomplete response retaining items already delivered as
`response.output_item.done`; unfinished items are discarded. One request runs at a time
per chat WebSocket, with at most one queued create. Named `stream_id` multiplexing is
unsupported by the chat adapter. Connections and messages are bounded (128 concurrent
WebSockets, 64 MiB per message); compression is disabled. Shutdown closes open sockets.

Chat history is process-local memory shared between HTTP and WebSockets, with a
one-hour idle lifetime per response ID. A timer removes expired entries even without
new traffic. The shared cache is limited to 1 GiB of serialized history across all
chat providers; each snapshot is limited to 16 MiB. Least recently used snapshots are
evicted first. Oversized snapshots are not retained. Valid
incomplete responses retain only their returned output; failed streams are not cached.
Each branch has an independent snapshot. Only conversation items carry over: current
request instructions, tools, and generation settings apply to every request.

The gateway credential is the local authorization boundary. Cached IDs are random,
scoped to the exact provider/model, and available to holders of that gateway credential;
client metadata is not a separate tenant credential. History never writes to disk,
including for `store: false`; `store: true` persistence remains unsupported by the chat
adapter. Restart, expiry, and eviction can invalidate IDs. A missing ID returns
`previous_response_not_found`; recover by sending full input without
`previous_response_id`. Codex uses that error to retry its full context. The gateway
never retries failed paid model requests itself.

Native and external Responses WebSockets select an upstream from the first create
and relay Responses events, including prewarm and interruption, without translation.
Routing prefixes are removed from each model. A connection cannot change its upstream;
open another connection when switching providers. External handshakes use only provider
credentials and allowlisted protocol headers; native handshakes retain native account
credentials. Native Responses state remains managed by that upstream.
Valid upstream WebSocket close codes are forwarded with a bounded generic reason,
including the message-too-large code Codex uses for HTTP fallback.

Request and response bodies, credentials, and prompts are not logged.

## Verify

```sh
go test ./internal/...
go vet ./...
go build ./cmd/fanout
```

Live-provider compatibility and performance require separate bounded runs; unit tests
do not establish either.
