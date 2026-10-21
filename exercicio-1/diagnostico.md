# Exercício 1: Diagnóstico e correção do consumer SQS → Bedrock

A correção está em Go (a vaga é Go | Python). O comportamento é o mesmo que eu entregaria em Python:
os conceitos (partial batch failure, classificação de erros, idempotência) não dependem da linguagem.

Referência de linhas do código original (numeradas a partir de `import boto3, json, os` = L1):

```text
L3   def handler(event, context):
L4       bedrock = boto3.client('bedrock-runtime')
L5       sqs = boto3.client('sqs')
L6       queue_url = os.environ['QUEUE_URL']
L8       for record in event['Records']:
L9-11        body = json.loads(...); user_input = body['message']; session_id = body['session_id']
L13-22       response = bedrock.invoke_model(... 'max_tokens': 2048, 'messages': [{'role':'user', ...}] ...)
L24-25       result = json.loads(response['body'].read()); print(f'Resposta: {result}')
L27      return {'statusCode': 200}
```

---

## (a) Problemas encontrados e impacto em produção

Ordenados por impacto. Os quatro primeiros explicam os sintomas relatados ("timeouts intermitentes" e
"mensagens reprocessadas desnecessariamente").

| # | Problema | Onde | Impacto em produção |
|---|---|---|---|
| 1 | **Uma exceção derruba o lote inteiro.** Não há `try/except`; qualquer erro (throttling, JSON inválido, `KeyError`) aborta o loop e a Lambda falha. | L8–25 | O SQS reentrega **todas** as mensagens do lote, inclusive as já respondidas: respostas duplicadas para o usuário e tokens pagos de novo. Pior: uma única mensagem inválida faz o lote inteiro falhar a cada tentativa até `maxReceiveCount`, e **as mensagens boas do mesmo lote vão para a DLQ junto com ela**. Isso alimenta diretamente o acúmulo da DLQ do Exercício 2. |
| 2 | **Sem timeout e sem consciência do deadline.** O client usa os padrões do boto3 (read timeout de 60s e retry "legacy" com até 5 tentativas). | L4, L13 | Uma chamada lenta ao Bedrock (comum em pico/throttling) pode sozinha passar do timeout da Lambda. Quando a Lambda estoura o tempo, o lote inteiro volta para a fila (problema 1 de novo). É a causa mais provável dos "timeouts intermitentes". |
| 3 | **Processamento sequencial do lote.** | L8 | Com lote de 10 e 3–8s por chamada, a última mensagem espera todas as anteriores: 30–80s de Lambda e latência enorme para quem caiu no fim do lote. No pico os lotes enchem, e isso explica parte dos **picos de 12s**. Também aumenta a chance de estourar o timeout (problema 2). |
| 4 | **Sem idempotência.** O SQS entrega *at-least-once*; nada impede processar a mesma mensagem duas vezes. | L8–25 | Com os problemas 1 e 2, cada reentrega gera nova chamada ao modelo, nova cobrança e, se a resposta fosse entregue, uma segunda resposta diferente para a mesma pergunta. O reprocessamento também consome cota do Bedrock e causa mais throttling, que causa mais reprocessamento: um ciclo que piora a latência no pico. |
| 5 | **A resposta nunca chega ao usuário.** O resultado só vai para `print`; `sqs` e `queue_url` são criados e nunca usados. | L5–6, L24–25 | O chatbot paga o modelo e descarta a resposta. Além disso, o `print` despeja o conteúdo inteiro da conversa no CloudWatch Logs: dado pessoal em log (LGPD) e custo de ingestão. |
| 6 | **Sem contexto de sessão.** `session_id` é lido e ignorado; só a mensagem atual vai ao modelo. | L11, L18 | O modelo responde cada mensagem como se fosse a primeira ("e o outro plano?" fica sem referência). A conversa perde coerência, que é justamente o que o sistema deveria garantir. |
| 7 | **Sem system prompt e com parâmetros que aumentam latência e variação.** Não há `system`; `max_tokens` é 2048; a temperatura fica no padrão do modelo (1.0). | L16–18 | Sem system prompt, o tom e as regras do negócio não existem (ligação direta com o Exercício 3). Temperatura alta aumenta a inconsistência entre respostas para a mesma pergunta. `max_tokens` alto permite respostas longas, e o tempo de geração cresce com os tokens de saída. |
| 8 | **Erros não classificados nem validados.** `KeyError` por campo ausente é tratado igual a throttling. | L9–11 | Mensagens que nunca vão funcionar (JSON inválido, campo faltando) são retentadas até `maxReceiveCount`, gastando invocações e atrasando as outras. |
| 9 | **Clients recriados a cada invocação.** | L4–5 | Cada invocação paga resolução de credenciais e handshake TLS de novo (latência de centenas de ms em cada lote) e não reaproveita o pool de conexões. |
| 10 | **Nenhuma observabilidade útil.** Sem log estruturado, sem correlation id, sem métricas de latência, tokens ou falhas. | todo | Impossível responder "o que aconteceu com a mensagem do cliente X?", "a latência vem do modelo ou da fila?" ou "quanto custa uma sessão?". O incidente de 12s não pode ser diagnosticado com o código como está. |

Menores: `os.environ['QUEUE_URL']` só falha em tempo de execução (deveria falhar na inicialização) e o
`modelId` está fixo no código (deveria ser configuração, para trocar de modelo ou usar inference profile
sem deploy de código).

---

## (b) O que foi corrigido e onde

| Problema | Correção | Onde (`exercicio-1/`) |
|---|---|---|
| 1. Lote inteiro reprocessado | `ProcessBatch` nunca retorna erro; cada falha vai individualmente em `BatchItemFailures`. Um `panic` em qualquer adaptador vira falha só daquela mensagem. | `codigo_corrigido.go`: `ProcessBatch`, `safeProcessMessage` |
| 2. Timeout / deadline | Timeout por chamada ao Bedrock (20s), timeout de conexão (2s), retry do SDK limitado a 2 tentativas (o retry principal é a reentrega do SQS). Antes de cada mensagem, checa o deadline do `ctx` da Lambda e devolve à fila o que não cabe no tempo restante. | `aws.go`: `BedrockChatModel.Complete`, `BuildAWSProcessor`; `codigo_corrigido.go`: `processSession` |
| 3. Sequencial | Sessões diferentes em paralelo (goroutines, limitadas por `MAX_WORKERS`). Mensagens da mesma sessão em ordem; se uma falha, as seguintes da mesma sessão voltam junto, com log `returned_preserving_session_order`. | `codigo_corrigido.go`: `ProcessBatch`, `processSession` |
| 4. Idempotência | Chave determinística `SentTimestamp#messageId`. Antes de chamar o modelo, verifica se a troca já existe; se sim, republica a resposta guardada sem gastar tokens. Gravação condicional (`attribute_not_exists`) resolve corrida entre entregas simultâneas. | `codigo_corrigido.go`: `ExchangeKey`, `processMessage`; `aws.go`: `DynamoSessionStore.SaveExchange` |
| 5. Resposta perdida | Resposta publicada numa fila de saída (FIFO: `MessageGroupId` = sessão, dedup = `messageId`). Logs registram só tamanhos e ids. | `aws.go`: `SQSPublisher.Publish`; `observability.go`: `NewLogger` |
| 6. Sem contexto | Últimas 10 trocas da sessão (DynamoDB, só as anteriores à mensagem atual) injetadas no prompt, cortando as mais antigas acima de 12.000 caracteres. | `codigo_corrigido.go`: `BuildMessages`; `aws.go`: `DynamoSessionStore.RecentExchanges` |
| 7. Prompt/parâmetros | `system` enviado (configurável via `SYSTEM_PROMPT_PATH`, para usar o prompt do Ex. 3), `max_tokens` 1024, temperatura 0.3, tudo configurável. `prompt_version` em cada log e métrica. | `aws.go`: `BedrockChatModel.Complete`; `codigo_corrigido.go`: `DefaultSettings` |
| 8. Classificação de erros | Validação na entrada com motivo explícito; mensagens inválidas e `ValidationException` do Bedrock vão direto para a DLQ com o motivo como atributo. Throttling, timeout e indisponibilidade voltam à fila. Sem DLQ configurada, a mensagem volta à fila (nunca é descartada). | `codigo_corrigido.go`: `ParseRecord`, `quarantine`; `aws.go`: `SQSPoisonSink.Send` |
| 9. Clients por invocação | Clients criados uma vez por container, na fase INIT. | `codigo_corrigido.go`: `main`; `aws.go`: `BuildAWSProcessor` |
| 10. Observabilidade | Logs JSON (`log/slog`) com `session_id`, `message_id`, `correlation_id`, `receive_count`, latência, tokens, `stop_reason`, `prompt_version`. Métricas via CloudWatch EMF (sem chamada de rede no caminho crítico). | `observability.go`: `NewLogger`, `Metrics.Flush` |

### Métricas emitidas e alarmes sugeridos

| Métrica | Para que serve |
|---|---|
| `EndToEndLatency` | Tempo desde o envio do usuário (`SentTimestamp`), **incluindo a espera na fila**. |
| `ModelLatency` | Tempo só da chamada ao Bedrock. Comparada com a anterior, separa "modelo lento" de "fila acumulada". É o primeiro passo para diagnosticar os picos de 12s. |
| `MessagesProcessed`, `MessagesRetried`, `MessagesRejected`, `MessagesFailed` | Saúde do processamento. |
| `DuplicatesSkipped`, `DeadlineReturns`, `OrderReturns` | Quanto a idempotência, o deadline e a ordem por sessão estão atuando. |
| `InputTokens`, `OutputTokens`, `TruncatedResponses` | Custo e respostas cortadas por `max_tokens`. |

Dimensões: `Service` e `PromptVersion` (permite comparar antes/depois de uma mudança de prompt, ver Ex. 3).

Alarmes:
- DLQ com `ApproximateNumberOfMessagesVisible > 0`;
- p95 de `EndToEndLatency` > 8s por 5 min;
- `MessagesRetried / MessagesProcessed` > 5%;
- `ApproximateAgeOfOldestMessage` da fila principal > 30s (fila acumulando).

### Configuração da AWS que acompanha o código

O código sozinho não resolve. Estes ajustes de infraestrutura fazem parte da correção:

- **`FunctionResponseTypes: ["ReportBatchItemFailures"]`** no event source mapping. Sem isso, o SQS
  ignora o `batchItemFailures` e o problema 1 continua.
- **Visibility timeout da fila ≥ 6× o timeout da Lambda** (recomendação da AWS). Ex.: Lambda 60s →
  visibility 360s. Evita que uma mensagem ainda em processamento reapareça para outra invocação.
- **`maxReceiveCount` = 5** na redrive policy: absorve throttling passageiro sem prender a mensagem para
  sempre.
- **`MaximumConcurrency` no event source mapping**, dimensionada pela cota do Bedrock. Regra prática:
  `concorrência × MAX_WORKERS × (60 / latência média em s)` ≤ cota de requisições por minuto. Sem esse
  limite, um pico de mensagens escala a Lambda, estoura a cota, gera throttling e, com o retry, piora a
  latência de todos.
- **`MaximumBatchingWindowInSeconds` = 0**: uma janela de batching adiciona espera antes do
  processamento, o que é ruim para um chatbot.
- **DLQ com retenção de 14 dias** (o máximo), dando tempo para investigar antes de perder mensagens.
- **Tabela DynamoDB:** PK `session_id` (S), SK `sk` (S), TTL em `expires_at`, on-demand.
- **IAM mínimo:** `bedrock:InvokeModel` no modelo usado; `dynamodb:GetItem/Query/PutItem` na tabela;
  `sqs:SendMessage` na fila de resposta e na DLQ (a leitura da fila de entrada é feita pelo event source
  mapping).

Deploy: runtime `provided.al2023`, arquitetura arm64 (mais barata e com cold start menor), binário
chamado `bootstrap`:

```bash
GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o bootstrap . && zip function.zip bootstrap
```

### Outras alavancas para a latência (fora do código)

- **Prompt caching do Bedrock** (para modelos que suportam): o system prompt e o início do histórico se
  repetem a cada chamada; com cache, o prefill fica mais rápido e mais barato.
- **Modelo mais recente/rápido ou cross-region inference profile:** mais capacidade em pico e menos
  throttling. O `MODEL_ID` já é configurável.
- **Provisioned concurrency** se cold start aparecer no p99 (em Go/arm64 costuma ser pequeno).

### Limites desta entrega

- Os adaptadores AWS foram testados com fakes que imitam os clients do SDK, **não contra a AWS real**
  (o desafio permite). Não validei IAM, a configuração do event source mapping nem a latência real do
  Bedrock.
- A deduplicação da resposta fora da janela de 5 min do FIFO depende de o consumidor da fila de saída
  deduplicar por `in_reply_to` (suposição documentada no README).
- Se duas entregas da mesma mensagem rodarem ao mesmo tempo, as duas chamam o modelo; só uma resposta é
  gravada e publicada. É um custo raro de tokens, aceito para não precisar de lock distribuído.

---

## (c) Trade-off: `invoke_model` vs `invoke_model_with_response_stream`

**O ponto central:** o streaming reduz o **tempo até o primeiro token** (latência percebida), não o tempo
total de geração. Ele só melhora a experiência se os tokens chegarem ao usuário enquanto são gerados. Com
SQS na entrada, a Lambda **não tem conexão aberta com o usuário**: ela recebe a mensagem de uma fila e
devolve a resposta para outra fila.

| Aspecto | `invoke_model` (síncrono) | `invoke_model_with_response_stream` |
|---|---|---|
| Latência percebida | O usuário espera a resposta inteira. | Pode começar a ler em ~1s, **mas só se houver um canal de streaming até ele**. Atrás de SQS, o ganho é zero. |
| Tempo total / custo do Bedrock | Igual. | Igual (mesmos tokens). A Lambda fica ocupada o mesmo tempo. |
| Entrega ao usuário | Uma mensagem na fila de saída. | Exige um canal em tempo real (API Gateway WebSocket com `PostToConnection` por chunk, AppSync, SSE). São muitas chamadas por resposta, gerenciamento de conexões e reconexão. |
| Falha no meio | A falha é atômica: a mensagem volta à fila e a resposta é refeita do zero. | O usuário já viu metade da resposta. Uma nova tentativa gera um texto diferente, então é preciso decidir entre descartar o parcial (confuso) ou continuar (difícil). O retry via SQS perde o sentido. |
| Idempotência | Simples: grava a troca completa e republica se for reentregue. | Só existe resposta completa no fim do stream; reentregas no meio são difíceis de deduplicar. |
| Guardrails / moderação | Avalia o texto completo antes de entregar. | Precisa moderar pedaços parciais (risco de mostrar algo antes de bloquear) ou bufferizar, o que anula o ganho. |
| Observabilidade | Uso de tokens e `stop_reason` vêm na resposta. | Chegam só no evento final do stream; o tratamento de erro é por evento. |
| Canal | Funciona com qualquer canal. | Canais como WhatsApp e SMS **não aceitam mensagem parcial**: o streaming é inútil lá. |

**Recomendação:**
1. **Manter `invoke_model` no caminho SQS.** Para o chatbot nesse desenho, o streaming adiciona
   complexidade operacional (canal em tempo real, falhas parciais, moderação de pedaços) sem ganho
   para o usuário.
2. **Atacar a latência real primeiro:** paralelismo, timeouts, fim do reprocessamento, `max_tokens`
   menor e controle de concorrência contra throttling. Isso reduz o tempo **total**, que é o que todos os
   canais sentem.
3. **Se o produto tiver um canal web/app interativo** e a latência percebida for prioridade, criar um
   **caminho síncrono separado** para ele: cliente → API Gateway WebSocket ou Lambda Function URL com
   response streaming → Bedrock com streaming. A fila SQS continua para canais assíncronos (WhatsApp,
   e-mail) e para o processamento de fundo. É a decisão certa por canal, não uma troca global.

---

## Trade-off de arquitetura do código

Escolhi **um único pacote `main`**, com uma interface por dependência externa (`ChatModel`,
`SessionStore`, `Publisher`, `PoisonSink`), em vez de uma arquitetura hexagonal com vários pacotes.

- **A favor:** o avaliador lê o fluxo inteiro em `codigo_corrigido.go` e compara com o original. As
  interfaces permitem testar tudo sem AWS. Em Go, interfaces pequenas definidas por quem usa são o idioma
  da linguagem.
- **Contra:** se o consumer crescer (vários modelos com roteamento, várias filas, regras de negócio), um
  pacote só vira um arquivo grande demais. Aí valeria separar em `internal/` por responsabilidade.
- **Para ~1.250 linhas (incluindo adaptadores e simulação local) e quatro dependências,** mais camadas
  seriam cerimônia sem benefício. A separação por arquivo (`aws.go`, `observability.go`, `local.go`) já
  isola o que muda por motivos diferentes.
