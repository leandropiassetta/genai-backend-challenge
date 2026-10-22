# Exercício 3c: Qual métrica priorizar para detectar regressão de qualidade

**Escolha: a taxa de escalonamento para humano**, segmentada por intenção e acompanhada de duas métricas
de guarda.

## Por que ela, e não as outras duas

| Métrica | O que mede | Detecta regressão de qualidade de um prompt? |
|---|---|---|
| Latência média | Eficiência | **Não.** Um prompt pior pode até deixar a resposta mais rápida (respostas mais curtas, menos perguntas de esclarecimento). Uma mudança que faz o bot cancelar uma compra sem perguntar **reduz** a latência e é exatamente a regressão que queremos pegar. |
| Custo por sessão | Eficiência | **Não.** Segue a mesma lógica: menos turnos e menos tokens podem significar que o bot está resolvendo menos, ou resolvendo errado. |
| **Taxa de escalonamento** | **Se o bot resolveu ou desistiu** | **Sim.** É a única das três ligada ao resultado da conversa. Num chatbot de suporte de marketplace, escalar significa que o bot não resolveu, o que custa um atendente humano e piora a experiência. Quando um prompt piora o entendimento, o usuário se frustra e pede uma pessoa. |

Latência e custo continuam sendo monitorados, mas como **guarda-corpos** (o prompt novo é maior, então
mais tokens de entrada; o prompt caching compensa), não como sinal de qualidade.

## Por que ela não basta sozinha

A taxa de escalonamento pode se mover pelo motivo errado, nas duas direções:

- **Cair por regressão:** se o bot passa a cancelar direto, sem perguntar, as conversas de cancelamento
  ficam curtas e "resolvidas", sem escalonamento. A métrica melhora enquanto o produto piora (compras
  ou vendas erradas canceladas, afetando também a outra parte da transação).
- **Subir por melhoria:** o prompt novo manda oferecer um atendente quando o bot não sabe uma política.
  Escalar em vez de inventar uma resposta é o comportamento **certo**, e aumenta a taxa.

Por isso, ela vem acompanhada de:

1. **Segmentação por intenção.** A taxa geral dilui o efeito: cancelamento é uma fração das conversas. A
   comparação é feita **por intenção** (cancelamento de compra, de venda, de conta, rastreio de pedido,
   devolução...), usando a classificação já registrada nas conversas.
2. **Métricas de guarda de resultado:**
   - **Recontato em 24–72h sobre o mesmo assunto:** o usuário voltou porque não foi resolvido. Pega a
     "resolução falsa" que a queda de escalonamento esconderia.
   - **Cancelamentos contestados em até 7 dias:** o usuário (ou a outra parte da transação) reclama de
     um cancelamento que não pediu, ou tenta refazer a compra cancelada. É o sinal direto de cancelamento
     errado e a métrica de dano para a mudança deste exercício.
   - **Taxa de "não, não é isso"** depois de uma pergunta ou confirmação do bot: mostra quantas vezes o
     bot interpretou errado o objeto.

## Como usar na prática

1. **Antes do deploy:** a suíte de `casos_de_teste.md` roda e bloqueia a publicação se falhar. É o sinal
   antecipado, porque as métricas de produção demoram a responder.
2. **No deploy:** canário por `prompt_version` (ex.: 10% do tráfego no prompt novo). O consumer do Ex. 1
   já registra `prompt_version` em todos os logs e métricas, o que permite comparar as duas versões no
   mesmo período, com o mesmo perfil de clientes, em vez de comparar "semana passada vs esta semana".
3. **Volume necessário:** para detectar uma piora de 3 pontos percentuais numa taxa de escalonamento de
   ~20% (95% de confiança, 80% de poder), são necessárias cerca de **3 mil conversas por versão** no
   segmento analisado. Para o segmento de cancelamento, isso pode levar dias: mais um motivo para os
   testes offline virem antes.
4. **Alarme:** escalonamento da intenção "cancelamento" na versão nova acima do baseline + 3 p.p., **ou**
   qualquer aumento significativo em cancelamentos contestados → rollback para a versão anterior (trocar a
   `PROMPT_VERSION`/`SYSTEM_PROMPT_PATH` do consumer, sem deploy de código).

## Resumo

A taxa de escalonamento é a única das três métricas que observa **resultado**, e por isso é a prioridade.
Mas, lida sozinha, ela recompensa um bot que "resolve" rápido e errado. Segmentada por intenção, comparada
por `prompt_version` e cruzada com recontato e cancelamentos contestados, ela vira um detector confiável de
regressão de qualidade.
