# llm-fanout-proxy

A Go proxy that lets Codex use ChatGPT and other model providers in the same session.
It passes native Responses traffic through and translates Responses to Chat Completions
for providers that need it.

## Run it

With Go 1.24 or newer:

```sh
cp examples/fanout.yaml fanout.yaml
export FANOUT_API_KEY='replace-with-a-local-gateway-key'
export LITHOS_API_KEY='replace-with-your-provider-key'
go run ./cmd/fanout -config fanout.yaml -check
go run ./cmd/fanout -config fanout.yaml
```

The example listens on `127.0.0.1:8765` and uses Lithos as its external provider.
Edit [the YAML](examples/fanout.yaml) to use a different provider. Keep API keys in
the environment and put their variable names in `api_key_env`. The gateway key is
separate from the provider keys.

Set the provider's `protocol` to `responses` for passthrough or
`chat_completions` for translation.

## Connect Codex

Add this to your Codex `config.toml`:

```toml
[model_providers.fanout]
name = "Fanout"
base_url = "http://127.0.0.1:8765/backend-api/codex"
wire_api = "responses"
requires_openai_auth = true
supports_websockets = false
env_http_headers = { "x-litellm-api-key" = "FANOUT_API_KEY" }
```

From a shell with the same `FANOUT_API_KEY`, start a new session:

```sh
codex -c 'model_provider="fanout"'
```

### With codexx

Build the binary with `go build -o ~/.local/bin/llm-fanout-proxy ./cmd/fanout`
and put the YAML at `~/.config/fanout.yaml`. Keep `api_key_env: FANOUT_API_KEY`
in the YAML. The launcher generates that key and reads provider exports from
`~/.config/fanout.env` if present. Use mode `0600` for that file.

Run `codexx --fanout`. After replacing the binary or changing transport settings,
run `codexx --fanout app-server daemon restart` and start a new session.

## Model routing

Unprefixed native model IDs and IDs starting with `chatgpt/` go to ChatGPT.
An ID starting with a configured prefix like `lithos/` goes to that provider,
with the prefix stripped. Each request goes to one upstream. HTTP can switch
upstreams between requests. A WebSocket stays with the upstream selected on
its first request.

The proxy merges the native and external model catalogs on demand. Use
`model_overrides` to set context windows or reasoning levels for upstream model
IDs. The first matching pattern wins.

## Chat Completions limits

The adapter handles streaming, tool calls, and incremental requests. Its
history cache lives in memory, with a 1 GiB total limit and a 16 MiB limit per
conversation. Response IDs expire after one hour of inactivity. If the proxy
returns `previous_response_not_found`, resend the full context without
`previous_response_id`. Clients that share a gateway key share access to that
cache.

External collaboration calls use plaintext. The adapter rejects encrypted-only
reasoning, opaque compaction, and unsupported hosted tools. It passes custom
tool grammars to the model as instructions.

## Checks

`/health/readiness` checks whether the proxy is up. `/stats` returns history
cache counters.

```sh
go test ./internal/...
go vet ./...
go build ./cmd/fanout
```
