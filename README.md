# openclaw-api

Bridge HTTP em Go para o OpenClaw Gateway.

Mantem dois modos:

- `POST /v1/chat` (sincrono, responde na mesma requisicao)
- `POST /v1/jobs` + `GET /v1/jobs/{id}` (assincrono, com callback opcional)

## Variaveis de ambiente

- `OPENCLAW_TOKEN` (obrigatoria, alternativa a `OPENCLAW_TOKEN_FILE`)
- `OPENCLAW_TOKEN_FILE` (caminho de arquivo com token)
- `OPENCLAW_UPSTREAM_URL` (default: `http://127.0.0.1:18789`)
- `OPENCLAW_SCOPES` (default: `operator.write,operator.read`)
- `PORT` (default: `8080`)

## Rodar local

```bash
go mod tidy
OPENCLAW_TOKEN='SEU_TOKEN' go run .
```

## Endpoints

- `GET /health`
- `POST /v1/chat`
- `POST /v1/jobs`
- `GET /v1/jobs/{id}`

## Exemplo chat (sincrono)

```bash
curl --location 'http://localhost:8080/v1/chat' \
  --header 'Content-Type: application/json' \
  --data '{
    "user_id":"alanweb7",
    "agent_id":"main",
    "session_key":"agent:main:http:1234",
    "message":"Meu nome e Alan. Qual e meu nome?"
  }'
```

## Exemplo jobs (assincrono)

```bash
curl --location 'http://localhost:8080/v1/jobs' \
  --header 'Content-Type: application/json' \
  --data '{
    "user_id":"alanweb7",
    "agent_id":"main",
    "session_key":"agent:main:http:1234",
    "message":"Qual nome eu te pedi para lembrar?",
    "callback_url":"https://webhook.site/SEU-ID",
    "callback_headers":{"X-Source":"openclaw-api"}
  }'
```

Resposta:

```json
{"id":"job_xxx","status":"queued"}
```

Consultar:

```bash
curl --location 'http://localhost:8080/v1/jobs/job_xxx'
```

## Docker

```bash
docker build -t openclaw-api:latest .
docker run --rm -p 8080:8080 \
  -e OPENCLAW_TOKEN='SEU_TOKEN' \
  -e OPENCLAW_UPSTREAM_URL='http://127.0.0.1:18789' \
  openclaw-api:latest
```

## Docker Swarm + Traefik (subdominio)

```bash
export OPENCLAW_API_IMAGE='ghcr.io/SEU_USER/openclaw-api:latest'
export OPENCLAW_API_HOST='openclaw-api.pullse.ia.br'
export OPENCLAW_UPSTREAM_URL='http://127.0.0.1:18789'
export OPENCLAW_TOKEN='SEU_TOKEN'
export OPENCLAW_SCOPES='operator.write,operator.read'
export TRAEFIK_CERTRESOLVER='letsencrypt'

docker stack deploy -c stack.yml openclaw
```

Teste:

```bash
curl --location 'https://openclaw-api.pullse.ia.br/v1/chat' \
  --header 'Content-Type: application/json' \
  --data '{
    "user_id":"alanweb7",
    "agent_id":"main",
    "session_key":"agent:main:http:1234",
    "message":"Qual nome eu te pedi para lembrar?"
  }'
```

## Docker Swarm + Traefik (PathPrefix)

```bash
export OPENCLAW_API_IMAGE='ghcr.io/SEU_USER/openclaw-api:latest'
export OPENCLAW_API_HOST='openclaw-api.pullse.ia.br'
export OPENCLAW_API_PREFIX='/openclaw-api'
export OPENCLAW_UPSTREAM_URL='http://127.0.0.1:18789'
export OPENCLAW_TOKEN='SEU_TOKEN'
export OPENCLAW_SCOPES='operator.write,operator.read'
export TRAEFIK_CERTRESOLVER='letsencrypt'

docker stack deploy -c stack.pathprefix.yml openclaw
```

Teste:

```bash
curl --location 'https://openclaw-api.pullse.ia.br/openclaw-api/v1/chat' \
  --header 'Content-Type: application/json' \
  --data '{
    "user_id":"alanweb7",
    "agent_id":"main",
    "session_key":"agent:main:http:1234",
    "message":"Qual nome eu te pedi para lembrar?"
  }'
```

## Observacao

Jobs sao armazenados em memoria do processo. Se o container reiniciar, o historico de jobs e perdido.


mais exemplos de uso

```bash
curl -X POST "https://openclaw-api.pullse.ia.br/openclaw-api/v1/jobs" \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer <TOKEN>" \
  -d '{
    "user_id":"alanweb7",
    "agent_id":"main",
    "session_key":"agent:main:main",
    "message":"Teste webhook",
    "callback_url":"https://seu-webhook.com/openclaw",
    "callback_headers":{"X-Source":"openclaw-api"}
  }'
```