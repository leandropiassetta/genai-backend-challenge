# Avaliação Técnica: Desenvolvedor Go | Python + IA

Respostas ao desafio da Talent Studio (UDS Tecnologia): um chatbot com IA generativa que recebe mensagens
via AWS SQS, orquestra chamadas ao modelo (AWS Bedrock) em Lambda e mantém contexto de sessão, com três
problemas em produção: latência alta, falha silenciosa no reprocessamento da DLQ e respostas
inconsistentes para pedidos ambíguos de cancelamento.

## Estrutura

| Caminho | Conteúdo |
|---|---|
| [`exercicio-1/`](exercicio-1/) | Consumer SQS → Bedrock corrigido, em **Go** |
| [`exercicio-1/codigo_corrigido.go`](exercicio-1/codigo_corrigido.go) | Validação, montagem do prompt com histórico e orquestração do lote |
| [`exercicio-1/aws.go`](exercicio-1/aws.go) | Adaptadores Bedrock, DynamoDB e SQS |
| [`exercicio-1/observability.go`](exercicio-1/observability.go) | Logs JSON e métricas CloudWatch EMF |
| [`exercicio-1/local.go`](exercicio-1/local.go) | Implementações em memória e simulação local (**mock, sem AWS**) |
| [`exercicio-1/diagnostico.md`](exercicio-1/diagnostico.md) | Problemas, correções, configuração da AWS e trade-off de streaming |
| [`exercicio-2/analise.md`](exercicio-2/analise.md) | Causas da falha silenciosa no n8n, instrumentação e critérios de migração |
| [`exercicio-3/system_prompt.md`](exercicio-3/system_prompt.md) | Novo system prompt e justificativa das decisões |
| [`exercicio-3/casos_de_teste.md`](exercicio-3/casos_de_teste.md) | Plano de testes de comportamento (13 casos, N execuções cada) |
| [`exercicio-3/analise_metricas.md`](exercicio-3/analise_metricas.md) | Métrica priorizada para detectar regressão de qualidade |

## Como rodar

Requisito: Go 1.24+. Nada acessa a AWS: os testes e a simulação usam implementações em memória.

```bash
cd exercicio-1
go test -race ./...     # testes unitários, com detector de condição de corrida
go run . -local         # simula um lote: sucesso, throttling, ordem por sessão, DLQ e reentrega
```

Build para a Lambda (runtime `provided.al2023`, arm64):

```bash
cd exercicio-1 && GOOS=linux GOARCH=arm64 go build -tags lambda.norpc -o bootstrap .
```

## Suposições sobre o sistema

- **Mensagem de entrada (SQS):** `{"session_id": string, "message": string, "correlation_id"?: string}`.
  A implementação preserva a ordem da sessão dentro de cada lote; ordem global entre invocações exigiria
  uma fila FIFO agrupada por `session_id` ou outro mecanismo de serialização.
- **Contexto de sessão:** tabela DynamoDB com PK `session_id`, SK `<SentTimestamp>#<messageId>`, um item
  por troca (pergunta + resposta) e TTL de 7 dias. A chave é determinística entre reentregas e serve
  também de chave de idempotência.
- **Entrega da resposta:** o consumer publica numa fila SQS de saída, consumida pelo serviço do canal
  (WebSocket, app etc.). Esse serviço deduplica por `in_reply_to` fora da janela de 5 min do SQS FIFO.
- **Modelo:** Claude via Bedrock `InvokeModel` (formato Anthropic Messages). O `MODEL_ID` é configurável e
  o padrão é o mesmo do código original.
- **Negócio (Ex. 3):** o enunciado não diz qual é o negócio. Assumi um **marketplace de compra e venda de
  usados**, em que "quero cancelar" pode ser uma compra, uma venda, um anúncio ou a conta. As políticas
  de cancelamento entram por variável. O procedimento do prompt independe do domínio.
- **n8n (Ex. 2):** os nós de receive/delete chamam `ReceiveMessage`/`DeleteMessage` do SQS e o
  `/reprocess` reenvia a mensagem para a fila principal.

## Linha de raciocínio

**Exercício 1.** Parti dos sintomas ("timeouts intermitentes" e "mensagens reprocessadas") e procurei no
código o que os produz. A raiz é uma só: qualquer erro, ou uma Lambda que estoura o tempo, faz o SQS
reentregar **o lote inteiro**. Isso duplica respostas e custo, consome cota do Bedrock (o que gera mais
throttling e mais latência) e, quando há uma mensagem inválida no lote, leva as mensagens boas junto para
a DLQ. A correção combina partial batch failure, classificação de erros (transitório volta à fila,
permanente vai direto para a DLQ com o motivo), timeout por chamada e consciência do deadline,
paralelismo entre sessões com ordem preservada dentro do lote, idempotência por chave determinística,
contexto de sessão no prompt e observabilidade que separa a latência do modelo da espera na fila. Escrevi
em Go, com uma interface por dependência externa, para testar tudo sem AWS. Detalhes e trade-offs em
[`diagnostico.md`](exercicio-1/diagnostico.md).

**Exercício 2.** "Acumula sem erro visível" só tem duas explicações: o fluxo lê e não apaga, ou a DLQ
enche mais rápido do que é drenada. O backlog visível, a idade da mensagem mais antiga e a relação entre
mensagens recebidas e apagadas ajudam a separar as duas; não usei `NumberOfMessagesSent` como entrada do
redrive automático porque essa movimentação não entra na métrica. Ordenei as causas pela chance de **não
gerarem erro**:

- o ramo `false` do IF sem destino (agravado pelo `Body` do SQS ser string não parseada);
- o `ReceiptHandle` perdido entre nós;
- o `DeleteMessage` com handle vencido, que retorna sucesso sem apagar.

A instrumentação adiciona saídas laterais e observação externa sem mudar o caminho feliz. Para este
caso, migraria o reprocessamento para AWS nativo (dado pessoal, caminho crítico, falha silenciosa) e
manteria o n8n para automação operacional com decisão humana. Detalhes em
[`analise.md`](exercicio-2/analise.md).

**Exercício 3.** A inconsistência de ~30% existe porque o prompt original não decide o que fazer diante
de "quero cancelar" sem objeto, então o modelo escolhe a cada vez. O novo prompt transforma a escolha em
procedimento (identificar → esclarecer com uma pergunta → verificar a política → confirmar → agir), com
critério verificável de "inequívoco" e os itens do usuário injetados no contexto. O "fora do tom" vira
comportamento concreto (nem precipitado, nem obstrutivo). Como o prompt reduz variação mas não garante,
a confirmação também é exigida no backend. Os testes rodam cada caso 20 vezes, já que o problema é
estatístico, e incluem o prompt antigo como controle. A métrica priorizada é a taxa de escalonamento,
segmentada por intenção e cruzada com recontato e cancelamentos contestados, porque latência e custo
podem melhorar justamente quando a qualidade piora. Detalhes em
[`system_prompt.md`](exercicio-3/system_prompt.md),
[`casos_de_teste.md`](exercicio-3/casos_de_teste.md) e
[`analise_metricas.md`](exercicio-3/analise_metricas.md).

## Pergunta de follow-up

> Se pudesse endereçar apenas um dos três problemas primeiro em produção, qual e por quê?

**O problema 1, atacado pela correção do consumer** (partial batch failure, timeouts, controle de
concorrência e idempotência). Ele forma um ciclo de falha que se autoamplifica e alimenta o acúmulo na
DLQ. O problema de prompt é independente e continua importante, mas afeta uma intenção específica.

| Critério | Por que o problema 1 primeiro |
|---|---|
| **Impacto no usuário final** | Atinge **todos** os usuários no horário de pico, e não um subconjunto. Os outros dois afetam fatias menores: as mensagens que já falharam (DLQ) e as conversas de cancelamento ambíguas (prompt). |
| **Risco operacional** | O consumer **alimenta o problema 2**: cada lote reprocessado leva mensagens boas para a DLQ. Também se **autoamplifica**: reprocessar consome cota do Bedrock, que gera throttling, que gera mais timeouts e mais reprocessamento. Enquanto ele não for corrigido, drenar a DLQ só devolve as mensagens para o mesmo ciclo. |
| **Esforço** | Moderado e concentrado: envolve a Lambda, o event source mapping e dependências de estado/saída. Eu separaria o rollout: primeiro partial batch failure, timeouts e limite de concorrência; depois contexto e idempotência persistente. O deploy pode ser canário e cada etapa tem rollback claro. |

**O que faço com os outros dois enquanto isso** (contenção barata, não solução):

- **DLQ:** retenção em 14 dias e alarme em `ApproximateAgeOfOldestMessage`. Em fila standard, a expiração
  continua baseada no timestamp original, mesmo que a métrica de idade da DLQ conte desde a movimentação.
  Depois que o consumer estiver corrigido, usar o **redrive nativo** do SQS, sem depender do n8n.
- **Prompt:** uma trava no backend que só executa um cancelamento após confirmação explícita na conversa.
  É uma contenção menor do que o rollout completo do prompt e reduz o risco de dano real enquanto a nova
  versão passa pela suíte de testes.

**O que mudaria minha escolha:** se os dados mostrassem que o prompt está **executando cancelamentos
errados** em produção (dano real à outra parte da transação, não só uma pergunta a mais), eu conteria
isso primeiro com a trava de confirmação no backend, por ser uma mudança pontual, e seguiria com o
consumer em seguida.

## Uso de IA

**Ferramentas:** Claude Code (Anthropic) na construção inicial e ChatGPT/Codex (OpenAI) na revisão crítica
final.

**Onde a IA ajudou:**
- O Claude Code auxiliou a decompor o enunciado, propor a arquitetura do consumer, rascunhar o código Go,
  os testes e a simulação local, além das primeiras versões dos documentos dos três exercícios.
- O ChatGPT/Codex comparou a entrega final com o PDF, revisou criticamente código e documentação,
  executou testes com detector de corrida, `go vet` e build Linux/arm64 e apontou afirmações que precisavam
  ser limitadas ou corrigidas.

**O que eu conduzi, modifiquei ou rejeitei:**
- Rejeitei uma implementação inicial direta em Python sem alinhamento prévio e escolhi Go para demonstrar
  aderência à vaga. Mantive interfaces pequenas para permitir testes sem AWS e recusei adicionar IaC e um
  ambiente AWS fictício que eu não conseguiria validar nesta entrega.
- Adaptei o Exercício 3 de exemplos genéricos/telecom para um marketplace de usados, declarando essa escolha
  como suposição. Também rejeitei deixar segurança apenas no prompt: a confirmação precisa ser validada pelo
  backend.
- Na revisão final, corrigi a sugestão de usar `NumberOfMessagesSent` para medir o redrive automático da
  DLQ, pois a AWS não contabiliza essa movimentação nessa métrica. Também restringi a afirmação de ordem por
  sessão ao lote atual e deixei claro que a gravação condicional não impede duas chamadas concorrentes ao
  modelo.
- Rejeitei tratar 20 execuções sem erro como prova de taxa zero. Esse resultado serve como gate de release;
  a garantia contra cancelamentos indevidos continua sendo responsabilidade do backend.

**Minha revisão por exercício:**

- **Exercício 1:** percorri os caminhos de sucesso, erro transitório, mensagem inválida, reentrega e
  publicação. A solução está executável e bem testada, mas eu não afirmaria ordem global por sessão nem
  idempotência sem custo sob concorrência. Antes de produção, ainda validaria IAM, event source mapping e
  os adaptadores contra uma conta AWS de teste.
- **Exercício 2:** tratei as causas como hipóteses ordenadas, não como diagnóstico fechado, porque o workflow
  real do n8n não foi fornecido. Corrigi a leitura das métricas da DLQ e manteria o n8n fora do caminho de
  dados crítico, usando-o apenas para alerta e aprovação operacional.
- **Exercício 3:** conferi se cada regra do prompt tem caso de teste correspondente e mantive a decisão de
  medir comportamento repetido, não uma resposta isolada. O ponto que eu não delegaria ao modelo é a ação
  irreversível: confirmação, elegibilidade e id do item devem ser verificados no backend.

**Verificações feitas:**
- `go test -race ./...`, `go vet` e o build para Linux/arm64 em cada etapa de código.
- As referências a funções no `diagnostico.md` foram conferidas contra o código.
- O cálculo de tamanho de amostra da análise de métricas foi recalculado.

Os adaptadores AWS foram testados com fakes, não contra a AWS real.
