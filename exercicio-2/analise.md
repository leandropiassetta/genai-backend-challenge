# Exercício 2: Fluxo n8n + DLQ, diagnóstico de falha silenciosa

Fluxo analisado:

```text
Schedule Trigger → AWS SQS (receive) → IF (mensagem válida?) → HTTP Request (POST /reprocess) → SQS Delete Message
```

Sintoma: as mensagens continuam acumulando na DLQ com o fluxo ativo, e o painel do n8n não mostra erros.

**Suposições** (não há acesso ao fluxo real):
- O "receive" e o "delete" são feitos por nós que chamam `ReceiveMessage` e `DeleteMessage` da API do
  SQS. Dependendo da versão do n8n, isso pode ser o nó nativo ou um HTTP Request assinado com credencial
  AWS. As causas abaixo valem para os dois.
- `/reprocess` é um endpoint interno que reenvia a mensagem para a fila principal (ou a processa direto).

**Leitura do sintoma:** "acumulando sem erro visível" tem só duas explicações possíveis.
1. **O fluxo não está apagando o que lê,** sem gerar erro.
2. **A DLQ está recebendo mais rápido do que o fluxo consegue drenar.**

A primeira coisa a fazer é separar as duas com as métricas da própria DLQ no CloudWatch (veja "Como
confirmar" em cada causa).

---

## (a) Causas mais prováveis

Ordenadas da mais para a menos provável, considerando que **não aparece erro**. Causas que geram erro
explícito (como falta de permissão) só ficam silenciosas se algo estiver engolindo o erro, por isso
aparecem mais abaixo.

### 1. O ramo "falso" do IF não leva a lugar nenhum
- **O que acontece:** mensagens consideradas inválidas saem pela saída `false` do IF, que não está
  conectada. O n8n simplesmente para de processar esses itens. Não é erro: a execução termina como
  **sucesso**. A mensagem nunca é apagada, volta a ficar visível depois do visibility timeout e é lida de
  novo na próxima execução, para sempre.
- **Por que é tão provável:** o `Body` de uma mensagem SQS é uma **string** JSON. Se o IF testa algo como
  `{{$json.Body.session_id}}` sem fazer o parse antes, o campo é sempre vazio e **todas** as mensagens caem
  no `false`. O fluxo "roda" e não faz nada. Comparações com tipo divergente (número vs string) no nó IF
  também podem desviar itens, conforme a configuração de validação de tipo.
- **Como confirmar:** na lista de execuções, abrir uma execução e ver quantos itens saíram em cada ramo do
  IF. No SQS, o atributo `ApproximateReceiveCount` das mensagens da DLQ cresce a cada execução: elas são
  lidas e nunca apagadas.
- **Correção:** fazer o parse do `Body` antes do IF. Dar destino ao ramo `false`: mover para uma fila de
  quarentena ("parking lot") com o motivo e alertar. Uma mensagem inválida que fica na DLQ para sempre
  também atrapalha a leitura das válidas.

### 2. O `ReceiptHandle` se perde no caminho até o Delete
- **O que acontece:** no n8n, cada nó substitui o JSON do item pela sua própria saída. Depois do HTTP
  Request, `{{$json}}` é a **resposta do `/reprocess`**, não mais a mensagem do SQS. Se o nó Delete usa
  `{{$json.ReceiptHandle}}`, recebe vazio. Isso só aparece como erro se o nó não estiver configurado
  para continuar em caso de falha (causa 4).
- **Como confirmar:** inspecionar a entrada do nó Delete numa execução e ver o valor real do
  `ReceiptHandle`.
- **Correção:** referenciar o nó de origem explicitamente (`{{ $('AWS SQS').item.json.ReceiptHandle }}`)
  ou preservar o handle no item, por exemplo com a opção do HTTP Request de incluir a resposta num campo
  sem descartar a entrada.

### 3. Visibility timeout menor que o tempo do fluxo: o Delete "funciona" e não apaga nada
- **O que acontece:** ao receber, a mensagem fica invisível pelo visibility timeout **da DLQ** (padrão:
  30s). Se o `/reprocess` demora (ou a execução fica na fila do n8n), o tempo expira e a mensagem volta a
  ficar visível. Ela pode até ser recebida de novo por outra execução, ganhando um `ReceiptHandle` novo. A
  documentação do SQS diz que o `DeleteMessage` precisa do handle **mais recente**; com um handle antigo, a
  requisição **retorna sucesso, mas a mensagem não é apagada**. É a definição exata de falha silenciosa.
- **Também gera duplicidade:** a mensagem reaparece e é reprocessada de novo, o que é crítico se o
  `/reprocess` não for idempotente.
- **Como confirmar:** comparar a duração das execuções (e do `/reprocess`) com o visibility timeout da
  DLQ. Ver `ApproximateNumberOfMessagesNotVisible` alto (mensagens "em voo") e `NumberOfMessagesDeleted`
  menor que `NumberOfMessagesReceived`.
- **Correção:** visibility timeout no receive (parâmetro `VisibilityTimeout`) maior que o pior caso do
  fluxo, com folga. Processar e apagar item a item, não em lote no final. Timeout explícito no HTTP Request.

### 4. Erros engolidos pela configuração dos nós ou pelo `/reprocess`
- **O que acontece:** "On Error: Continue" (ou o antigo "Continue On Fail") ligado no HTTP Request ou no
  Delete faz o erro virar um item comum, e a execução aparece como sucesso. Variante: o `/reprocess`
  responde **2xx** sem ter processado (aceita com `202` e falha depois, ou devolve `200` com
  `{"error": ...}` no corpo).
- **Como confirmar:** checar a configuração "On Error" de cada nó; procurar itens com campo `error` na
  saída; cruzar com os logs do `/reprocess` no mesmo horário.
- **Correção:** só seguir para o Delete com sucesso **real** (status 2xx **e** corpo validado). Erros vão
  para a saída de erro do nó com alerta (ver item b).

### 5. Throughput menor que a entrada: a DLQ enche mais rápido do que é drenada
- **O que acontece:** o `ReceiveMessage` devolve por padrão **1 mensagem** por chamada
  (`MaxNumberOfMessages`, máximo 10). Com *short polling* (`WaitTimeSeconds` = 0), ele consulta só uma
  amostra dos servidores e pode voltar **vazio mesmo com mensagens na fila**. Com um schedule de alguns
  minutos, a drenagem é de poucas mensagens por hora. Enquanto isso, o consumer do Exercício 1 manda lotes
  inteiros para a DLQ (incluindo as mensagens boas que estavam no mesmo lote de uma inválida).
- **Como confirmar:** comparar, na DLQ, `NumberOfMessagesSent` (entrada) com `NumberOfMessagesDeleted`
  (saída) por hora.
- **Correção:** a de verdade é na origem (Exercício 1: partial batch failure e fim do reprocessamento do
  lote inteiro). No fluxo: `MaxNumberOfMessages=10`, long polling (`WaitTimeSeconds=20`) e repetir o
  receive enquanto houver mensagens.

### 6. Loop de reprocessamento: a mensagem volta para a DLQ
- **O que acontece:** o `/reprocess` reenvia para a fila principal **sem que a causa da falha tenha sido
  corrigida**. O consumer falha de novo e, depois de `maxReceiveCount` tentativas, a mensagem volta para a
  DLQ. O fluxo "funciona" (lê, reenvia, apaga), mas o saldo da DLQ não diminui. Mensagens que nunca vão
  funcionar (*poison messages*) ficam circulando para sempre e gastando tokens a cada volta.
- **Como confirmar:** as mesmas mensagens (pelo `session_id`/conteúdo, ou por um atributo de contagem de
  reprocessamento) reaparecem na DLQ depois de reprocessadas.
- **Correção:** contar os reprocessamentos num atributo da mensagem e, acima de um limite, mandar para a
  quarentena em vez de reenviar. Não reprocessar enquanto a causa raiz do consumer não estiver corrigida.

### 7. Permissões IAM ou KMS
- **O que acontece:** a credencial do n8n tem `sqs:ReceiveMessage` mas não `sqs:DeleteMessage`. Ou a DLQ é
  criptografada com SSE-KMS e falta `kms:Decrypt` (para ler) ou, no `/reprocess`, `kms:GenerateDataKey`
  (para enviar à fila principal criptografada). Isso gera `AccessDenied`, **que só some do painel se a
  causa 4 estiver presente**.
- **Como confirmar:** CloudTrail com eventos de dados do SQS mostra `DeleteMessage` com `AccessDenied`;
  o *IAM policy simulator* para a credencial usada.
- **Correção:** política mínima com `sqs:ReceiveMessage`, `sqs:DeleteMessage`,
  `sqs:ChangeMessageVisibility` e `sqs:GetQueueAttributes` na DLQ, e `kms:Decrypt` na chave da fila.

### 8. Idempotência do `/reprocess`
- **Sem idempotência:** como as causas 2 e 3 fazem a mesma mensagem ser reprocessada várias vezes, cada
  repetição gera uma nova resposta ao usuário e um novo custo de tokens.
- **Com deduplicação mal feita:** o endpoint reconhece o id como "já processado" e devolve 2xx sem fazer
  nada, enquanto a primeira tentativa na verdade falhou. A mensagem é apagada e **perdida**.
- **Correção:** chave de idempotência estável (o `messageId` original, propagado como atributo), com
  estado "em andamento / concluído / falhou", e não só "já vi".

### 9. Fila, região ou conta errada
- **O que acontece:** a credencial ou a URL configurada no n8n aponta para outra fila (de staging, de
  outra região). O fluxo roda perfeitamente na fila errada.
- **Como confirmar:** conferir a URL completa (conta + região + nome) e o `NumberOfMessagesReceived` da DLQ
  certa: se é zero, o fluxo não está lendo dela.

### Agravante de urgência
Em filas padrão, a expiração de uma mensagem que vai para a DLQ conta a partir da **entrada original na
fila de origem**, não da chegada na DLQ. Com a retenção padrão de 4 dias, mensagens podem estar
**expirando e sendo perdidas** enquanto o fluxo falha. Medida imediata: retenção da DLQ em 14 dias (o
máximo) e alarme em `ApproximateAgeOfOldestMessage`.

---

## (b) Como instrumentar o fluxo sem alterar a lógica principal

Princípio: o caminho feliz (receive → IF → reprocess → delete) continua igual. Só adiciono **saídas
laterais** e **observação externa**, de modo que todo item termine num destino visível.

### Dentro do n8n
| Recurso | O que torna visível |
|---|---|
| **Error Workflow** (configuração do workflow) com um *Error Trigger* → Slack/PagerDuty | Qualquer execução que falhe passa a gerar alerta, com o nome do nó e a mensagem de erro. |
| **Saída de erro nos nós** HTTP Request e Delete ("On Error → Continue (using error output)") ligada a um ramo de log/alerta | Erros por item (timeout, 4xx/5xx, `AccessDenied`) deixam de ser engolidos, sem interromper os outros itens. |
| **Ramo `false` do IF** ligado a log/alerta com o motivo | Mensagens descartadas pela validação deixam de sumir em silêncio. A decisão de apagar ou não continua a mesma. |
| **Salvar execuções** de sucesso e de falha (com retenção limitada) | Permite abrir qualquer execução e ver item por item o que entrou e saiu de cada nó. |
| **Contadores por execução** num ramo final: recebidas, válidas, inválidas, reprocessadas com sucesso, falhas, apagadas. Enviados ao CloudWatch (`PutMetricData`) ou ao Datadog | Mostra tendência. A divergência "recebidas ≠ apagadas" vira gráfico e alarme. |
| **Correlation id**: enviar o `MessageId` original como header (`X-Correlation-Id`) para o `/reprocess` | Liga a execução do n8n aos logs do endpoint e do consumer. |

### Fora do n8n (o que pega até o fluxo parado)
| Recurso | O que torna visível |
|---|---|
| Alarme `ApproximateNumberOfMessagesVisible > 0` na DLQ por 15 min | A DLQ não está sendo drenada. |
| Alarme `ApproximateAgeOfOldestMessage` > 1h | Mensagens envelhecendo, com risco de expirar. |
| Metric math: `NumberOfMessagesReceived > 0` **e** `NumberOfMessagesDeleted = 0` em 15 min | **Lê e não apaga:** a assinatura exata das causas 1–4. |
| Alarme em `NumberOfMessagesSent` da DLQ acima do normal | A entrada disparou: o problema está no consumer (Ex. 1), não no n8n. |
| **Heartbeat** ("dead man's switch"): o último nó faz ping num serviço como Healthchecks.io ou Cronitor; sem ping em 2 intervalos, alerta | Detecta o fluxo que **parou de rodar** (desativado, n8n fora do ar, credencial expirada), que nenhum alarme interno pega. |
| **CloudTrail com eventos de dados do SQS** | Cada `DeleteMessage` com resultado e erro, inclusive `AccessDenied`. |
| **Canário sintético**: a cada hora, inserir na DLQ uma mensagem marcada como teste e verificar se ela some em N minutos | Testa o fluxo de ponta a ponta continuamente, inclusive o delete. |

---

## (c) Decisão de design: quando trocar o n8n por uma solução nativa AWS

### Critérios técnicos
| Critério | Mantém n8n | Migra para AWS nativo |
|---|---|---|
| **Volume / vazão** | Dezenas de mensagens por dia | Centenas+ por hora, ou picos: o polling por schedule não acompanha |
| **Latência de recuperação** | Horas são aceitáveis | Minutos (ex.: o usuário ainda está na conversa) |
| **Garantias de entrega** | Perder ou duplicar uma mensagem é tolerável | Precisa de retry com backoff, partial batch failure e idempotência; o event source mapping já faz o poll e o delete corretamente |
| **Observabilidade** | Painel do n8n basta | Precisa de métricas, tracing (X-Ray) e alarmes no mesmo lugar que o resto do sistema |
| **Dados sensíveis** | Sem PII no payload | As mensagens têm conversas de clientes: no n8n elas ficam gravadas nos **dados de execução**, fora da política de retenção/LGPD do sistema |
| **Credenciais** | — | O n8n costuma usar access key de longa duração; a Lambda usa role IAM temporária com permissão mínima |
| **Mudança e qualidade** | Fluxo visual, alterado por operações | Precisa de code review, testes, versionamento e IaC (o fluxo n8n é difícil de revisar em diff) |
| **Ownership** | O time de operações é dono e mantém | O time de engenharia é dono do caminho crítico |
| **Interação humana** | Aprovação manual, triagem, integração com Slack/Jira | Processo totalmente automático |
| **Custo** | Instância do n8n já existe | Pagamento por uso, sem servidor para manter |

### Opções nativas, da mais simples para a mais flexível
1. **Redrive nativo da DLQ do SQS** (console "Start DLQ redrive" ou API `StartMessageMoveTask`):
   devolve as mensagens para a fila de origem **sem nenhum código**, com controle de taxa. Serve quando a
   causa raiz já foi corrigida e basta reenviar. **É o paliativo imediato deste incidente.**
2. **Lambda com event source mapping na DLQ:** quando o reprocessamento tem lógica (validar, enriquecer,
   decidir entre reenviar ou quarentenar). Herda partial batch failure, concorrência máxima, uma DLQ
   própria para o que falhar de novo, testes e IaC: o mesmo padrão do Exercício 1.
3. **EventBridge Pipes** (DLQ → filtro → enriquecimento → destino): o filtro declarativo substitui o IF; o
   Pipes cuida de poll, delete e retry. Bom quando a lógica é mais roteamento do que código.
4. **Step Functions:** quando o reprocessamento tem várias etapas ou aprovação humana com espera.

### Minha decisão para este cenário
**Migraria o caminho de reprocessamento.** São mensagens de clientes (dado pessoal), num fluxo crítico,
que falhou em silêncio: exatamente os critérios em que o n8n perde.

**Ordem:**
1. **Agora:** corrigir o consumer (Ex. 1), subir a retenção da DLQ para 14 dias e usar o redrive nativo.
2. **Em seguida:** Lambda na DLQ com regras explícitas de quarentena.

**Manteria o n8n onde ele é forte:** um fluxo que lê as **métricas e os motivos** da quarentena e publica
um resumo no Slack, com um botão de "aprovar redrive" para casos que exigem decisão humana. Automação de
operação, não caminho de dados.

**Um ponto de produto vale levantar com o time:** reprocessar uma mensagem de chatbot horas depois pode
gerar uma resposta fora de contexto, porque o usuário já saiu da conversa. O reprocessamento deve
considerar a idade da mensagem. Acima de um limite (ex.: 30 min), é melhor acionar um atendente ou mandar
uma mensagem de retomada do que responder como se nada tivesse acontecido.
