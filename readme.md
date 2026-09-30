# Ping Jobs

Bot HTTP em Go que recebe vagas de emprego e publica cada vaga nos canais do Discord configurados. Ele não recebe comandos nem conversa no Discord: usa a API REST somente para publicar embeds.

## Requisitos

- Go 1.22 ou superior
- Uma aplicação e um bot criados no [Discord Developer Portal](https://discord.com/developers/applications)
- Permissões `Send Messages` e `Embed Links` para o bot em cada canal configurado

## Configuração

No `.env`, `DISCORD_DESTINATIONS` deve ficar em uma única linha. Cada destino usa `guild_id:channel_id`; separe vários destinos por vírgula, sem espaços ou aspas. Os IDs abaixo são fictícios:

```dotenv
DISCORD_BOT_TOKEN=token-do-bot
API_TOKEN=um-token-secreto-para-a-api
DISCORD_DESTINATIONS=111111111111111111:222222222222222222,333333333333333333:444444444444444444
HTTP_PORT=8080
```

Configure as variáveis de ambiente:

| Variável | Obrigatória | Padrão | Uso |
| --- | --- | --- | --- |
| `DISCORD_BOT_TOKEN` | Sim | — | Token do bot no Discord Developer Portal |
| `API_TOKEN` | Sim | — | Token Bearer exigido pelo endpoint HTTP |
| `DISCORD_DESTINATIONS` | Sim | — | Uma linha com pares `guild_id:channel_id` separados por vírgula |
| `HTTP_ADDR` | Não | `:8080` | Endereço de escuta do servidor HTTP |
| `HTTP_PORT` | Não | `8080` | Porta publicada no host pelo Docker Compose |

Exemplo:

```sh
export DISCORD_BOT_TOKEN='token-do-bot'
export API_TOKEN='um-token-secreto-para-a-api'
export DISCORD_DESTINATIONS='123456789012345678:234567890123456789'
go run ./cmd/pingbot
```

Para carregar essas variáveis diretamente de `.env` no shell antes de executar localmente: `set -a; . ./.env; set +a`.

O bot precisa estar no servidor correspondente ao `guild_id`, e o `channel_id` precisa pertencer a esse servidor.

## Docker Compose

Copie o arquivo de exemplo, preencha os IDs e tokens, e inicie o serviço:

```sh
cp -n .env.example .env
# Edite .env
docker compose -f compose.yml up --build -d
```

Por padrão, a API fica disponível na porta `8080`. Para usar outra porta no host, altere `HTTP_PORT` no `.env`. O serviço reinicia automaticamente após falhas ou reinicializações do Docker.

## API

### `POST /messages`

Cabeçalho obrigatório:

```text
Authorization: Bearer <API_TOKEN>
Content-Type: application/json
```

Corpo (os campos `date` são recebidos como texto e exibidos como enviados):

```json
{
  "title": "Desenvolvedor Go",
  "content": "Descrição e requisitos da vaga.",
  "link": "https://exemplo.com/vagas/desenvolvedor-go",
  "date": "2026-09-29"
}
```

O endpoint envia a vaga a todos os canais listados em `DISCORD_DESTINATIONS`. No Discord, o título fica clicável e aponta para `link`; `content` aparece na descrição e `date` em um campo de data.

Resposta `202 Accepted` quando a mensagem entra na fila:

```json
{
  "id": "id-da-mensagem",
  "expires_at": "2026-09-29T15:30:00Z",
  "destinations": 1
}
```

O endpoint confirma que a vaga entrou na fila, não que já foi publicada. O bot tenta cada canal configurado; se algum envio falhar, tenta novamente apenas os destinos pendentes. A vaga fica em memória por no máximo 10 minutos e é removida quando todos os envios têm sucesso ou quando expira. Se o processo parar, a fila em memória é perdida. O limite é de 1000 vagas pendentes. Os limites dos campos são: título 256 caracteres, conteúdo 4096, link 2048 e data 1024.

Exemplo com `curl`:

```sh
curl -X POST http://localhost:8080/messages \
  -H 'Authorization: Bearer um-token-secreto-para-a-api' \
  -H 'Content-Type: application/json' \
  -d '{"title":"Desenvolvedor Go","content":"Descrição e requisitos da vaga.","link":"https://exemplo.com/vagas/desenvolvedor-go","date":"2026-09-29"}'
```

### `GET /healthz`

Retorna `204 No Content` para indicar que o processo HTTP está ativo.
