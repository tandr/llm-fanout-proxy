# llm-fanout-proxy

A small Go gateway for Codex: native Responses forwarding, on-demand model discovery,
and Responses-to-Chat-Completions translation for external providers. HTTP and protocol
handling use the standard library; YAML parsing uses one dependency. Each request
selects one upstream; requests are never broadcast.

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
directly. The listener defaults to loopback. `/health/readiness` reports readiness.

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
`supports_websockets = false`, and the gateway header supplied through the environment:

```toml
[model_providers.fanout]
name = "Local Go gateway"
base_url = "http://127.0.0.1:8765/backend-api/codex"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
env_http_headers = { "x-litellm-api-key" = "FANOUT_API_KEY" }
```

Select with `-c 'model_provider="fanout"'` for a new session. Existing sessions can be
bound to their original provider ID; replacing that provider's endpoint requires
an explicit configuration change. Building this repository does not change the
installed launcher, start a daemon, or activate the gateway.

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
Select it with `codex --fanout`; it has a separate `fanout` provider ID and
`fanout.sock` control socket. `codex --litellm` keeps its existing LiteLLM route,
configuration, and runtime. Normal `codex` and `codex --pure` continue using the
native route. Existing profiles and running servers are unchanged.

## Translation boundaries

The Chat Completions adapter is stateless: Codex HTTP requests supply full conversation
history. It translates function tools, namespaced tools, freeform custom tools, text,
reasoning, usage, and streaming output. Custom-tool grammars are provided to the model
as instructions; they are not enforced by the gateway.

Opaque compaction, encrypted-only reasoning, `previous_response_id`, and unsupported
hosted tools fail explicitly. Native Responses traffic is forwarded without applying
those translation restrictions. Model capabilities must reflect the selected provider;
discovering a model ID alone does not establish vision or hosted-tool support.

Client cancellation cancels upstream work. Failed requests are not automatically
retried. Request and response bodies, credentials, and prompts are not logged.

## Verify

```sh
go test ./internal/...
go vet ./...
go build ./cmd/fanout
```

Live-provider compatibility and performance require separate bounded runs; unit tests
do not establish either.
