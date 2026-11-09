# Vidyom — Especificação Completa do Projeto

> **Open-source AI-powered video workflow engine**  
> **Preserve · Discover · Transform · Share**  
> **Documento-base de arquitetura, produto e implementação**  
> Versão do documento: **1.1 — 08/11/2026** (revisão de consistência e lacunas sobre a v1.0; ver **§20 Registro de revisão v1.1**)  
> Origem do projeto: **MapaDeva — https://www.mapadeva.com.br/**  
> Situação: **especificação para implementação; não representa funcionalidades já entregues**.

---

## 0. Como ler este documento

Este documento consolida **todas as decisões expressas ao longo da conversa de definição do Vidyom** e os detalhes técnicos propostos para transformá-las em implementação. Para evitar confusão:

- **[DECIDIDO]**: requisito ou escolha explicitamente estabelecida na conversa.
- **[PROPOSTO]**: desenho técnico recomendado nas discussões; ajustar durante o desenho detalhado, sem contrariar os requisitos decididos.
- **[FUTURO]**: extensão planejada, que deve ser possível pela arquitetura, mas não precisa estar implementada no MVP.
- **[VALIDAR]**: compatibilidade, limitação, custo, licenciamento ou comportamento de uma ferramenta externa que deverá ser confirmado em uma prova de conceito antes de ser assumido.

Na revisão v1.1, trechos alterados ou acrescentados estão marcados com **(v1.1, Rn)**, onde `Rn` remete ao item correspondente da §20. Quando a revisão resolveu uma ambiguidade que a conversa não decidiu, o resultado está marcado como **[PROPOSTO]**, nunca como [DECIDIDO]. Nenhuma decisão [DECIDIDO] da v1.0 foi revertida; apenas esclarecida ou tornada consistente.

Os nomes de comandos, contratos de plugins e formatos JSON contidos neste documento são **interfaces-alvo do Vidyom**, e não comandos que já existem em MediaCMS, VideoVortex, FFmpeg, YouTube ou n8n. Exemplos de IDs, nomes de buckets, URLs, caminhos e horários são fictícios.

---

## 1. Visão e princípios

### 1.1 Nome, identidade e origem

- **[DECIDIDO] Nome do projeto: `Vidyom`.**
- **[DECIDIDO]** O Vidyom nasce para servir ao **MapaDeva**, mas será **open source**, disponível e útil para qualquer pessoa ou organização.
- **[DECIDIDO]** O sistema não será especificamente religioso: deverá atender também a escolas, criadores de conteúdo, universidades, instituições culturais, empresas, comunidades e acervos pessoais.
- **[PROPOSTO]** Descrição internacional: **Open-source AI-powered video workflow engine**.
- **[PROPOSTO]** Lema: **Preserve · Discover · Transform · Share**.
- **[DECIDIDO]** Os executáveis serão nomeados `vidyom` (CLI), `vidyomd` (daemon) e, conforme arquitetura proposta, `vidyom-worker` (worker).
- **[PROPOSTO]** O README indicará “Originally built for MapaDeva”, sem criar dependência funcional ou de marca.
- A escolha do nome foi inspirada em **vídeo**, **vidya** (conhecimento) e **Om** (referência espiritual).

### 1.2 Problema a resolver

O MapaDeva produzirá gravações de aulas de ioga, cursos, encontros, palestras, entrevistas e outros conteúdos. Cada gravação original pode ter aproximadamente **10 GB**. É necessário:

1. Preservar a gravação bruta com integridade e custo de armazenamento controlado.
2. Manter uma versão de trabalho de boa qualidade **acessível diretamente** para produção contínua, sem recuperação do backup.
3. Transcrever e indexar o conteúdo de forma pesquisável por palavras e **semântica**.
4. Encontrar histórias, exemplos, ensinamentos ou assuntos por meio de **prompts em linguagem natural**, mesmo sem correspondência literal de palavras.
5. Criar **muitos cortes independentes** do mesmo original, inclusive meses ou anos depois da ingestão.
6. Tratar cada corte como **novo ativo**, com master próprio, backup, metadados, indexação e histórico.
7. Publicar material aprovado em plataformas de vídeo, inicialmente YouTube, mantendo uma **biblioteca pública** de conteúdo selecionado.
8. Automatizar o processo por **workflows JSON**, reutilizando nós e conectores, sem programar para criar cada novo workflow.
9. Operar remotamente via **daemon de longa duração + CLI cliente**, com segurança, auditoria e observabilidade.
10. Não depender de um provedor único: armazenamento, publicação, IA, catálogos e observabilidade devem ser extensíveis por conectores e plugins.

### 1.3 Princípios de arquitetura

- **CLI-first, daemon-centric, API-driven.**
- **Workflows declarativos** em JSON, com arquitetura de nós e conexões inspirada no n8n, sem exigir n8n instalado.
- **Portabilidade:** metadados e transcrições em formatos abertos; exportação/importação como requisito.
- **Separação entre fonte, processamento, catálogo e publicação.**
- **Master operacional nunca depende de backup de arquivamento para cortes.**
- **Segurança por padrão** e menor privilégio.
- **Conectores substituíveis**, identificados por papéis lógicos.
- **Plugins e hooks** sem alteração do core para extensões usuais.
- **Execução durável**, com retomada, idempotência, rastreabilidade e prevenção de efeitos externos duplicados.
- **Um ativo, várias versões, vários derivados e várias publicações.**
- **Biblioteca privada distinta da pública**, com publicação explícita, revisada e controlada.

### 1.4 Fora de escopo de pressuposições

- Não assumir que MediaCMS, VideoVortex ou Rewind forneçam de fábrica toda a semântica especificada aqui.
- Não afirmar que armazenamento GCS Standard seja gratuito: não tem taxa específica de recuperação como classes frias, mas **armazenamento, operações e tráfego/egress podem custar**.
- Não confundir redução de tamanho do arquivo de áudio com redução proporcional dos **tokens** ou do preço de uma API de transcrição.
- Não pressupor que uma API do YouTube permita download genérico de arquivos publicados; a estratégia preferida é **manter o arquivo enviado ao YouTube** no storage operacional.
- Não disponibilizar conteúdo privado automaticamente na biblioteca pública.

---

## 2. Vocabulário e modelo editorial

### 2.1 Ativos

- **Original (`original`) [DECIDIDO]:** gravação de entrada, independentemente do tipo (aula, curso, encontro, palestra etc.). Substitui **definitivamente** a antiga pasta/categoria `palestras`. **(v1.1, R1)** Três conceitos distintos não devem ser confundidos: o **tipo de ativo** (`kind` = `original`), o **prefixo de armazenamento** (`originais/`, convenção do conector) e a **categoria de conteúdo** (`category` = `yoga_class`, `lecture` etc., §2.2). A decisão “`originais` e não `palestras`” refere-se ao tipo/prefixo; “palestra” passa a ser apenas uma categoria (`lecture`).
- **Corte (`clip`) [DECIDIDO]:** material derivado de um ou mais intervalos de outro ativo; recebe identidade, master e backup próprios.
- **Vídeo editado (`edited`) [DECIDIDO]:** outra versão editorial/derivada, com rastreabilidade à fonte. **(v1.1, R2) [PROPOSTO]** Vídeos editados fora do Vidyom (ex.: em um editor não-linear) entram por `ingest-derived.json` com os ativos-pai **declarados** pelo usuário; a linhagem fica marcada como `lineage.method = "declared"` (em contraste com `"computed"` para cortes gerados pelo Vidyom, cujos intervalos são exatos).
- **Master bruto (`source_original`):** arquivo de entrada na melhor forma preservada (ex.: 10 GB), guardado em backup.
- **Master de trabalho (`working_master`) [DECIDIDO]:** arquivo de boa qualidade mantido em storage operacional de acesso direto e permanente enquanto necessário à criação de cortes.
- **Proxy (`proxy`) [DECIDIDO]:** arquivo mais leve, usado para reprodução, previews, navegação e identificação visual dos trechos.
- **Áudio para ASR (`transcription_audio`):** derivado mono/comprimido adequado à transcrição; pode ter retenção temporária. **(v1.1, R3)** Se for descartado e for necessária nova transcrição depois da ingestão, ele é regenerado a partir do **master de trabalho**, nunca do Archive (coerente com §3.2).
- **Master de corte (`clip_master`) [DECIDIDO]:** novo master, renderizado a partir do master operacional da fonte, não de um proxy degradado nem do Archive.
- **Publicação (`publication`) [DECIDIDO]:** registro independente de um ativo em um destino externo (YouTube, Vimeo etc.).
- **Arquivo publicado (`published_file`) (v1.1, R4) [PROPOSTO]:** o arquivo exato enviado a um destino. Fica no papel `master` (storage operacional permanente). Se for byte a byte idêntico ao master do ativo (mesmo SHA-256), não é duplicado: a publicação apenas referencia o master e o hash.

### 2.2 Categorias de originais

A categoria é **metadado**, não estrutura fixa: `yoga_class`, `course`, `meeting`, `lecture`, `interview`, `event`, `other`, com suporte a tipos novos por configuração.

### 2.3 Identificação e relações

- **[DECIDIDO]** Identificador permanente para cada ativo: exemplos `ORI-001`, `COR-001`.
- **[DECIDIDO]** Cada corte guarda o(s) ativo(s) de origem, intervalos de tempo, versões e histórico.
- **[DECIDIDO]** Uma palestra/aula original pode gerar N cortes ao longo de múltiplos momentos de produção, sem reexecutar a ingestão original.
- **[DECIDIDO]** Um ativo pode ser publicado em vários destinos, cada um com `remote_id`, URL, metadados e estado próprios.
- **[PROPOSTO]** Usar UUID/ULID como chave técnica imutável e IDs amigáveis como aliases humanos.
- **(v1.1, R5) [PROPOSTO] Prefixos e formato dos IDs amigáveis:** `ORI-` (original), `COR-` (corte/`clip`), `EDT-` (editado), `JOB-` (execução), `APR-` (aprovação), `PUB-` (publicação), `CLIENT-` (cliente registrado). Numeração sequencial por prefixo, com no mínimo 3 dígitos e sem limite superior (`ORI-1000` é válido). A alocação é transacional no catálogo canônico, para que dois cortes criados ao mesmo tempo nunca recebam o mesmo ID. Um ID amigável nunca é reutilizado, mesmo após exclusão. Os prefixos ficam em português por decisão de origem (`COR` = corte); a API aceita e devolve sempre o ID técnico e o alias.
- **(v1.1, R6) [PROPOSTO] Versões de ativo:** cada ativo tem versões imutáveis `v1`, `v2`…. Nova renderização (ex.: ajuste de início/fim após aprovação), nova transcodificação do master ou substituição de arquivo criam nova versão; edição apenas de metadados (título, tags) não cria versão de mídia, mas fica no histórico. Arquivos de uma versão nunca são sobrescritos; as chaves de storage incluem a versão (§14.1). Publicações registram qual versão foi enviada.
- **(v1.1, R7) Cortes de cortes:** como a definição de `clip` admite derivar de “outro ativo”, um derivado que atinja `ready_for_clipping` pode ser fonte de novo corte. **[PROPOSTO]** Para evitar perda de geração, os intervalos são convertidos para a linha do tempo do original raiz e renderizados a partir do master do original, quando existir e a edição for contínua; a linhagem registra a cadeia completa.

### 2.4 Exemplo de linhagem

```text
ORI-001 — Aula de Yoga (original indexado)
├── COR-001 — Explicação da respiração Ujjayi
│   ├── Publicação YouTube
│   └── Publicação Vimeo
├── COR-002 — História da mulher de 90 anos
└── COR-003 — Caso do amigo que não recebia abraço
```

### 2.5 Estados de ativo (v1.1, R8) [PROPOSTO]

A v1.0 definia os estados de **job** (§5.5) mas citava estados de **ativo** (`ready_for_clipping`, `editorial_review`) sem defini-los. Estado do ativo, estado do job e visibilidade são eixos independentes.

| Estado (`status`) | Aplica-se a | Significado |
|---|---|---|
| `registered` | todos | ID alocado; nenhum arquivo verificado ainda |
| `ingesting` | todos | workflow de ingestão em andamento |
| `ready_for_clipping` | original e derivados | condições de prontidão de §3.1 satisfeitas; pode ser fonte de cortes |
| `ingest_failed` | todos | ingestão terminou com erro; nunca é fonte de corte |
| `editorial_review` | derivados | ingerido e aguardando produção editorial/aprovação de publicação |
| `approved` | derivados | aprovado para publicação |
| `rejected` | derivados | rejeitado na revisão; mantido para histórico |
| `published` | derivados | ao menos uma publicação confirmada (detalhes ficam em cada `publication`) |
| `withdrawn` | derivados | retirado de todas as publicações e da biblioteca pública |
| `archived_only` | todos | master de trabalho removido por política; **não** é fonte de corte até restauração administrativa |

Sinalizadores complementares: `master_available` (verificado periodicamente por `verify-storage.json`), `backup_verified`, `catalog_sync` (estado da sincronização com MediaCMS/VideoVortex, §13.1). Se uma verificação detectar master ausente ou corrompido, `master_available = false` e o ativo deixa de ser aceito por `create-clip`, sem restauração automática.

`visibility` (`private`, `unlisted`, `public`) descreve somente a biblioteca pública e os destinos; ativos nascem `private`.

---

## 3. Dois workflows independentes e subworkflows

### 3.1 Workflow A: `ingest-original.json` [DECIDIDO]

**Objetivo:** receber original, proteger o bruto, gerar versões de trabalho, extrair conhecimento, armazenar e indexar, deixando-o elegível para cortes futuros.

**Entradas possíveis:** arquivo local do servidor, upload do CLI, arquivo previamente disponível em storage, ou vídeo próprio publicado externamente e importado de fonte autorizada. Para uploads remotos, o CLI deve transferir o conteúdo de maneira retomável, com checksum. A forma de referenciar cada tipo de entrada está em §5.4.

**Etapas funcionais:**

1. Receber arquivo e metadados básicos: título, categoria, data, palestrante/instrutor, direitos, idioma quando conhecidos.
2. Registrar ativo com ID permanente; validar arquivo, formato e checksum SHA-256.
3. Persistir a cópia de entrada durante a ingestão; nunca depender de um link externo transitório. **(v1.1, R9)** A cópia local de entrada só pode ser apagada depois que o backup no `archive` **e** o master no `master` estiverem verificados por checksum.
4. Enviar **bruto** para papel de storage `archive` e **verificar** backup.
5. Gerar, com FFmpeg, um **master operacional comprimido**, mas apropriado para recortes futuros. **(v1.1, R10)** O master, o proxy e o áudio de ASR devem manter a **mesma linha do tempo** do bruto (sem cortar início/fim, mesmo instante zero, timestamps normalizados). Depois de gerar cada um, `ffprobe` compara a duração com a do bruto e rejeita diferença acima de uma tolerância configurável (ex.: 1 frame ou 100 ms). É isso que permite usar os timestamps da transcrição diretamente no master e no proxy.
6. Enviar e verificar master no papel `master` (inicialmente GCS Standard).
7. Gerar proxy leve no papel `proxy` (inicialmente GCS Standard).
8. Extrair áudio otimizado para transcrição **diretamente do arquivo de melhor qualidade disponível**, evitando dupla compressão de áudio.
9. Opcionalmente usar VAD (detecção de atividade de voz) para excluir silêncios longos, preservando um mapa de timestamps entre áudio processado e vídeo.
10. Transcrever por motor configurável (inicialmente faster-whisper), com timestamps; guardar transcrição bruta e formatos abertos.
11. Gerar resumos, capítulos, assuntos/tags, identificação de histórias e embeddings por IA.
12. Indexar texto e vetores no catálogo canônico (PostgreSQL + busca textual + pgvector como proposta inicial) e, onde compatível, no VideoVortex.
13. Registrar/expor o ativo no **MediaCMS**, catálogo visual privado.
14. Validar a existência/integridade do master operacional e dos índices.
15. Somente então definir estado **`ready_for_clipping`**.
16. Emitir eventos (`original.indexed`, `asset.indexed`, `workflow.completed`) e registrar auditoria.

**Condição de prontidão obrigatória:** master de trabalho acessível diretamente; transcrição com timestamps; índice de busca válido; controles de integridade. Sem isso, o ativo não é candidato a corte.

**(v1.1, R11) Esclarecimentos sobre a prontidão:**

- A lista acima não incluía o backup, embora as etapas 4 e §15.1 o exijam. **O backup verificado no `archive` passa a fazer parte da condição de prontidão** (coerente com a decisão “preservar bruto em backup”).
- O índice exigido é o do **catálogo canônico** (PostgreSQL). A sincronização com MediaCMS (etapa 13) e VideoVortex (etapa 12) **não bloqueia** `ready_for_clipping`: falhas ali ficam em `catalog_sync = failed`, com retry e alerta. Isso aplica a regra de §13.1 de que nenhum desses produtos é fonte única de verdade.
- O proxy é necessário para a prévia do corte (§3.2, etapa 6), então também faz parte da condição.

Condição consolidada: `backup_verified` ∧ `master_available` (verificado) ∧ proxy disponível ∧ transcrição com timestamps ∧ índice textual e vetorial válidos no catálogo canônico ∧ linha do tempo validada (R10).

### 3.2 Workflow B: `create-clip.json` [DECIDIDO]

**Objetivo:** a qualquer momento, receber **ID de original já indexado + prompt** e produzir um novo corte independente com ciclo completo de produção e publicação.

**Exemplos reais de entrada da conversa:**

- “História da mulher de 90 anos que não tinha remorso”.
- “Caso do amigo que não recebia abraço”.
- “Explicação da respiração Ujjayi”.

**Etapas funcionais:**

1. Receber `source_id`, `prompt` e opções (`complete_story`, versão curta até duração-alvo ou melhores momentos).
2. Resolver exclusivamente um **ativo indexado** (`ready_for_clipping` e `master_available`). **(v1.1, R12)** No MVP, `create-clip` recebe **uma única fonte**. A definição de corte (§2.1) admite “um ou mais” ativos de origem; montagem com várias fontes fica como **[FUTURO]**, mas o modelo de linhagem já registra a fonte de cada intervalo (§12.3).
3. Fazer busca lexical e semântica **restrita ao ativo solicitado**; não exigir palavras literais.
4. Recuperar contexto anterior e posterior e estimar limites narrativos (introdução, desenvolvimento, conclusão).
5. Gerar candidatos com timestamps e pontuação de confiança; se ambíguo, solicitar seleção/revisão, não cortar aleatoriamente.
6. Gerar prévia a partir do proxy; permitir ajuste de início/fim.
7. Obter aprovação do corte proposto ou prosseguir sob política explícita de aprovação editorial. **(v1.1, R13)** Este é o **portão de seleção** (`approval.kind = "clip_selection"`), distinto do **portão de publicação** da etapa 13 (`approval.kind = "publication"`). Os dois são registros de aprovação separados, com responsável, data e versão aprovada. **[PROPOSTO]** Se um pedido gerar intervalos que se sobrepõem bastante a um corte já existente da mesma fonte, o sistema avisa e mostra o corte existente antes de prosseguir; não bloqueia.
8. Ler master de trabalho **somente do papel `master`**, fazendo cache temporário local ao worker; **não acessar `archive`**.
9. Renderizar com FFmpeg um novo master de corte com qualidade adequada e preservar mapa dos intervalos usados (um intervalo ou montagem de vários).
10. Registrar **novo ativo derivado**, com ID, versão, master e linhagem.
11. Chamar **`ingest-derived.json`** para backup do master de corte, upload de master e proxy ao storage operacional, atualização/indexação de texto, resumos, thumbnail e catálogo privado.
12. Executar produção editorial: título, descrição, legendas, thumbnail, revisão dos fatos e permissões.
13. Solicitar **aprovação obrigatória por padrão** antes de publicação pública.
14. Chamar **`publish-video.json`** com o parâmetro `destinations` (ex.: `["youtube-main"]`). **(v1.1, R14)** O nome canônico passa a ser `publish-video`; `publish-youtube` deixa de ser um workflow separado (no máximo um alias que chama `publish-video` com `destinations = ["youtube-main"]`).
15. Criar/atualizar registro de publicação e disponibilizar o corte **na biblioteca pública** após confirmação da publicação e do estado público.
16. Permitir novos cortes da mesma fonte a qualquer momento, sem refazer o Workflow A.

**Regra invariável:** se `master` estiver indisponível, a execução sinaliza erro/indisponibilidade; **jamais restaura automaticamente do Archive**. Restauração, se excepcionalmente necessária, será um fluxo administrativo separado e explicitamente autorizado.

### 3.3 Subworkflows reutilizáveis [DECIDIDO]

- `ingest-derived.json`: trata novo corte ou outro derivado como ativo independente; backup, master operacional, proxy, metadados, indexação, integridade, catálogo e eventos.
- `publish-video.json`: revisão e aprovação, upload, monitoramento do processamento remoto, registro de URL/ID, cópia de arquivo publicado e atualização da biblioteca pública. Recebe `destinations` como parâmetro (v1.1, R14).
- **[PROPOSTO]** `restore-archived.json` como operação administrativa separada, com controle especial de permissão, auditoria e custo.
- **[PROPOSTO]** `reindex-asset.json`, `refresh-publication.json`, `verify-storage.json`, `bulk-create-clips.json`.
- **(v1.1, R15) [PROPOSTO]** `unpublish-video.json`: retira a publicação de um ou mais destinos (ou muda a visibilidade para privada), atualiza o registro e remove o ativo da biblioteca pública (evento `publication.withdrawn`). A v1.0 não previa nenhum caminho de retirada, mas ele é necessário para cumprir pedidos de remoção (§17, item 6).
- **(v1.1, R16) [PROPOSTO]** `delete-asset.json`: exclusão controlada (permissão `asset.delete`), que exige retirar antes as publicações, respeita a retenção mínima do Archive (§7.2) e mantém na auditoria um registro de que o ativo existiu.

### 3.4 Quando um vídeo já está no YouTube [DECIDIDO]

- Se foi publicado pelo Vidyom, **reter no storage operacional a mesma versão enviada**; nunca depender do YouTube como repositório master.
- Se só existir no YouTube, importar **uma cópia autorizada**, armazená-la/cacheá-la de forma persistente no fluxo de ingestão e indexá-la antes de aceitar cortes.
- Downloads/recuperações do YouTube dependem de permissões, métodos oficiais e disponibilidade de arquivos. A YouTube Data API não será tratada como API universal de download.
- O sistema não buscará vídeo remoto sob demanda **durante** a geração do corte.
- Cache de processamento do worker é temporário; cópia operacional de longo prazo é obrigatória para `ready_for_clipping`.

---

## 4. Mídia, transcrição e custo computacional

### 4.1 Perfis de mídia [PROPOSTO]

| Artefato | Perfil inicial sugerido | Finalidade |
|---|---|---|
| Bruto | Sem alteração, ~10 GB típico | Preservação histórica no Archive |
| Master operacional | H.264/H.265, preferencialmente 1080p ou resolução relevante da fonte, qualidade alta | Recortes, reenquadramentos, renderização futura |
| Proxy | MP4 H.264 720p, qualidade mais baixa | Player, previews e navegação |
| Áudio para ASR | Mono 16 kHz, Opus ~24–32 kbps, conforme motor suportado | Transcrição eficiente |
| Transcrição | JSON estruturado + SRT/VTT + Markdown/texto | Busca, edição, exportação |

Não degradar para 720p o **único** master de produção quando forem necessários cortes 1080p, zoom e vídeos verticais. Avaliar preservação operacional de 4K conforme a origem e os objetivos editoriais.

**(v1.1, R17) Regras complementares dos perfis:**

- **Nunca aumentar a resolução:** o perfil `master-1080p` gera no máximo 1080p; fonte de 720p gera master de 720p. O nome do perfil indica o teto, não um valor fixo.
- **Áudio do master** em alta qualidade (ex.: AAC ≥ 192 kbps estéreo, ou cópia do áudio original quando o contêiner permitir), porque cortes publicados herdam esse áudio. A tabela acima só especificava o vídeo.
- Preservar a taxa de quadros e o espaço de cor da fonte, salvo regra explícita do perfil.
- Manter a linha do tempo da fonte (R10).
- Perfis são versionados (`profiles/*.json` com `version`); o ativo registra qual versão de perfil gerou cada arquivo.

### 4.2 FFmpeg: exemplos

Proxy compatível:

```bash
ffmpeg -i input.mp4 \
  -vf "scale=-2:720" \
  -c:v libx264 -preset slow -crf 28 \
  -c:a aac -b:a 96k \
  -movflags +faststart \
  proxy.mp4
```

Áudio para transcrição (gerar diretamente da melhor fonte):

```bash
ffmpeg -i input.mp4 \
  -vn -ac 1 -ar 16000 \
  -c:a libopus -b:a 24k \
  transcricao.opus
```

O motor de transcrição pode requerer MP3/WAV/FLAC ou segmentação por limites de API. A implementação deve ajustar formatos conforme as capacidades reais do conector.

### 4.3 Transcrição e tokens [DECIDIDO: entendimento técnico]

- Compactar de 10 GB para 20 MB não implica redução proporcional do custo de transcrição; custo pode ser por **minuto**, **tokens de áudio** ou infraestrutura local, dependendo do provedor.
- Compactação reduz armazenamento, transferência, I/O e necessidade de analisar quadros visuais.
- Redução de duração real enviada (ex.: remoção de longos silêncios via VAD) **pode** reduzir custo de modelos tarifados por duração, desde que a inteligibilidade e timestamps sejam preservados.
- Para LLM que analisa texto, custo depende do tamanho da transcrição/trechos enviados, não do tamanho do vídeo original.
- Indexar **uma vez** e consultar texto/embeddings posteriormente, evitando retranscrição repetida.
- Transcrição de corte pode ser derivada da fonte, mas quando há montagem/edição/alteração de fala, deve haver reconciliação de timestamps e, se necessário, retranscrição dos intervalos alterados.

### 4.4 Cortes semânticos: requisitos

- Recuperação semântica robusta (descrições aproximadas, paráfrases, nomes incompletos).
- Identificação de **história completa**, e não apenas da primeira ocorrência de uma palavra.
- Segmentação com contexto de fala, ajuste de cortes em limites naturais de frase.
- Preservar referência temporal entre original, proxy, transcrição, corte e publicação.
- Suportar corte contínuo e montagem de múltiplos intervalos com transparência na linhagem.
- Produzir MP4, SRT/VTT, thumbnail, sugestões de título/descrição e um manifesto de edição.
- Review/approval antes de publicação, por padrão.
- **[PROPOSTO]** Evidências para revisão: texto encontrado, timestamps e score de confiança; corte ambíguo exige intervenção humana.
- **(v1.1, R18) [PROPOSTO] Índice vetorial:** cada vetor guarda o identificador e a versão do modelo de embeddings e a estratégia de segmentação (`chunking`) usada. Trocar o modelo exige reindexação (`reindex-asset.json`) e não pode misturar vetores de modelos diferentes na mesma busca. A estratégia de segmentação (por frase, janela de N segundos com sobreposição, ou por capítulo) não tinha sido definida e entra em §17, item 4.

---

## 5. Arquitetura de runtime: CLI + daemon + workers

### 5.1 Componentes [DECIDIDO]

- **`vidyom`**: CLI cliente leve. Envia comandos, uploads, solicitações, acompanha logs/estado, aprova, consulta catálogo e publica.
- **`vidyomd`**: daemon permanente, que roda em background em servidor local ou remoto. API, autenticação, autorização, scheduler, engine de workflows, gerenciamento de jobs, eventos, hooks, plugins, conectores e estado.
- **`vidyom-worker` [PROPOSTO]**: executa trabalhos pesados e isoláveis (FFmpeg, ASR, IA). Pode começar no mesmo host e depois escalar horizontalmente.
- **PostgreSQL + pgvector [PROPOSTO]**: catálogo canônico, execução, eventos, metadados, auditoria, índices de texto/vetores.
- **Storage operacional e archive**: abstratos via conectores e papéis lógicos.

### 5.2 Referência arquitetural

**[DECIDIDO]** Experiência semelhante ao **Herdr**: processos persistem além da sessão do cliente; clientes se conectam, desconectam e reconectam. O Vidyom não precisa preservar pseudo-terminais: preserva **jobs, checkpoints, arquivos intermediários e estado dos nós**.

### 5.3 Comunicação

- CLI e daemon no mesmo host: **Unix Domain Socket** em Unix/macOS [PROPOSTO], com permissões restritas.
- Remoto: **SSH** (túnel até API/socket) e/ou **HTTPS/TLS 1.3**, idealmente mTLS em cenários apropriados [DECIDIDO: criptografia e segurança; PROPOSTO: mecanismos].
- API versionada com JSON; streaming de eventos/logs por SSE, WebSocket ou gRPC streaming [PROPOSTO, escolher implementação].
- Múltiplos clientes simultâneos podem acompanhar o mesmo job.
- CLI pode funcionar em macOS, Linux e Windows; daemon inicialmente Linux [PROPOSTO].
- Processamento e segredos externos ficam no servidor, não há necessidade de FFmpeg ou Whisper no CLI.
- **(v1.1, R19) [PROPOSTO] Identidade do cliente:** a v1.0 exigia autenticação e citava `security sessions` / `security revoke CLIENT-001`, mas não dizia como um cliente se registra. Proposta: no socket Unix local, identificar o usuário pelas credenciais do processo (`SO_PEERCRED`) e mapeá-lo a um papel. Remotamente, `vidyom server add` faz um **pareamento**: o admin gera no servidor um código de uso único com validade curta; o cliente gera um par de chaves, cuja chave privada fica no keyring do cliente, e recebe um certificado de cliente (mTLS) ou um token de sessão com validade e escopo definidos. Cada cliente pareado vira um `CLIENT-nnn` revogável. Via SSH, a identidade SSH é mapeada a um usuário do Vidyom e o pareamento continua obrigatório.

### 5.4 Upload remoto

Distinguir claramente:

- `--input file=/data/aula.mp4` = arquivo já acessível ao **servidor**.
- `vidyom asset upload ./aula.mp4` = arquivo no **cliente**; transmissão retomável (chunked/resumable), com checkpoint e checksum; somente depois iniciar ingestão.

**(v1.1, R20) [PROPOSTO] Referências de entrada unificadas.** A v1.0 usava `--input file=/data/aula.mp4` (servidor), `asset upload` (cliente) e `original import aula-yoga.mp4` (local não especificado). Para não haver ambiguidade, toda entrada de arquivo de workflow é um URI:

| Forma | Significado |
|---|---|
| `server:/data/aula.mp4` | arquivo no filesystem do servidor |
| `upload:UPL-001` | upload retomável já concluído e verificado |
| `storage://<papel>/<chave>` | objeto em um papel de storage configurado (nunca `archive` em `create-clip`) |
| `import:<conector>/<id>` | importação autorizada de fonte externa (ex.: vídeo próprio no YouTube, §3.4) |

- `vidyom original import <caminho>` trata `<caminho>` como **arquivo do cliente** (faz upload e depois ingere); `--server-path` indica arquivo do servidor.
- Caminhos `server:` ficam restritos a diretórios de entrada (`ingestRoots`) configurados no daemon, para evitar leitura arbitrária de arquivos do servidor; caminhos com `..` ou links simbólicos que saiam dessas raízes são rejeitados.
- O caminho sem prefixo em `--input file=` continua aceito como `server:`, por compatibilidade com os exemplos.

### 5.5 Jobs e persistência

Estados propostos: `queued`, `running`, `waiting_approval`, `retrying`, `paused`, `completed`, `failed`, `cancelled`.

- Um comando remoto retorna `job_id` imediatamente; o cliente pode fazer attach/detach.
- Ctrl+C ao acompanhar logs **desconecta**, não cancela o job.
- Checkpoints duráveis por nó; retries com backoff e limites; idempotência de upload/publicação.
- Reinício do daemon provoca reconciliação de nós in-flight, sem duplicar efeito externo.
- Em espera de aprovação, nenhum processo CLI precisa permanecer conectado.
- Workers declaram CPU, GPU, memória, disco e capacidade; scheduler escolhe worker compatível [PROPOSTO].
- **[PROPOSTO]** Retenção de registros de execução e limpeza segura de temporários configuráveis.
- **(v1.1, R21) Proveniência permanente:** a retenção de jobs (§10.4, 90 dias) não pode apagar a linhagem. Cada versão de ativo guarda permanentemente um **resumo de proveniência**: ID do job, ID/versão/hash do workflow, tipos e versões dos nós, versão do perfil de mídia, versões de FFmpeg/ASR/modelos de IA e checksums de entrada e saída. Esse resumo pertence ao ativo, não ao histórico de jobs.
- **(v1.1, R22)** `paused` precisa de comandos para entrar e sair do estado: `vidyom job pause` e `vidyom job resume` (§11.3). Pausar não interrompe um efeito externo já iniciado; ele termina e o job pausa no próximo checkpoint. `waiting_approval` sai do estado por `approval approve|reject` ou por timeout da política.
- **(v1.1, R23)** IDs `JOB-nnn` seguem a regra de R5 (alias amigável sobre um ULID).

### 5.6 Linguagens [PROPOSTO]

- **Go** para CLI, daemon, API e coordenação de workers (binários simples, concorrência, distribuição).
- FFmpeg/ffprobe como executáveis externos controlados.
- **Python** para transcrição/IA quando vantajoso, executado em worker/processo separado.
- Plugins em processo isolado, HTTP/gRPC, WASM em casos específicos; não depender de plugins `.so` nativos do Go.

---

## 6. Motor de workflows JSON inspirado no n8n

### 6.1 Princípios [DECIDIDO]

- Workflows são arquivos **JSON**, contendo nós, tipos, versões, parâmetros, conexões, entradas, saídas e configurações.
- Criar novo fluxo **sem escrever código**, desde que os nós e plugins necessários já existam.
- Referência conceitual: exportações de workflow do **n8n**; **não** exigir compatibilidade binária/exata nem instalação do n8n.
- Plugins podem adicionar tipos de nós e conectores.
- O daemon valida os JSONs contra schemas versionados e executa um grafo dirigido (DAG inicialmente, com `foreach` e condições controlados).
- Composição e **subworkflows** reutilizáveis são requisitos centrais.

### 6.2 Modelo do JSON [PROPOSTO]

Campos recomendados: `id`, `name`, `version`, `description`, `inputs`, `trigger`, `nodes`, `connections`, `hooks`, `settings`, `outputs`.

- `nodes[].id`: único por workflow.
- `nodes[].type`: nome versionado do tipo (`video.cut`, `storage.put`, `ai.transcribe`).
- `nodes[].typeVersion`: contrato de plugin/nó.
- `nodes[].parameters`: parâmetros validados por JSON Schema.
- `connections`: arestas da dependência/controle; nó executa apenas quando todas entradas exigidas estiverem prontas.
- `workflow.execute`: invoca subworkflow com isolamento de inputs e outputs.
- `flow.if`, `flow.foreach`, `flow.wait`: controle explícito.
- `${inputs.foo}`, `${nodes.id.output.field}`, `${event.field}`: expressão **declarativa restrita**, sem avaliação arbitrária de JavaScript ou shell.
- **(v1.1, R24)** Uma referência `${nodes.X...}` no nó Y **exige** que X seja ancestral de Y em `connections`; o validador rejeita referência a nó que não é ancestral (no exemplo de §6.3, `backup` usa `register` e por isso `register → backup` é obrigatório). Para escrever `${` literalmente, usa-se `$${`.
- **(v1.1, R25) [PROPOSTO]** Nó `approval.request` (portão humano): parâmetros `kind` (`clip_selection` | `publication` | outro), `assetId`, `approvers` (papel ou usuários), `timeoutSeconds` e `onTimeout` (`reject` | `fail` | `remind`). Coloca o job em `waiting_approval` e cria um registro `APR-nnn`. A v1.0 citava `requireApproval` e `waiting_approval`, mas não tinha nó que os implementasse.
- Workflows podem ter `trigger` por CLI, evento, webhook ou agendamento, quando os tipos forem implementados.
- `--dry-run`: valida sintaxe, esquemas, conectores, permissões, recursos e ordem, sem executar efeitos externos.

### 6.3 Exemplo ilustrativo: ingestão (formato proposto)

```json
{
  "id": "ingest-original",
  "name": "Ingest Original",
  "version": "1.0.0",
  "inputs": {
    "file": { "type": "file", "required": true },
    "title": { "type": "string", "required": true },
    "category": { "type": "string", "default": "other" }
  },
  "nodes": [
    {
      "id": "register",
      "type": "catalog.register",
      "typeVersion": 1,
      "parameters": {
        "kind": "original",
        "title": "${inputs.title}",
        "category": "${inputs.category}"
      }
    },
    {
      "id": "backup",
      "type": "storage.put",
      "typeVersion": 1,
      "parameters": {
        "storage": "archive",
        "source": "${inputs.file}",
        "key": "originais/${nodes.register.output.id}/original.mp4",
        "verifyChecksum": true
      }
    },
    {
      "id": "compress",
      "type": "video.transcode",
      "typeVersion": 1,
      "parameters": {
        "source": "${inputs.file}",
        "profile": "master-1080p"
      }
    },
    {
      "id": "store_master",
      "type": "storage.put",
      "typeVersion": 1,
      "parameters": {
        "storage": "master",
        "source": "${nodes.compress.output.file}",
        "key": "originais/${nodes.register.output.id}/master.mp4",
        "verifyChecksum": true
      }
    },
    {
      "id": "transcribe",
      "type": "ai.transcribe",
      "typeVersion": 1,
      "parameters": {
        "source": "${inputs.file}",
        "language": "pt"
      }
    },
    {
      "id": "index",
      "type": "ai.index",
      "typeVersion": 1,
      "parameters": { "transcript": "${nodes.transcribe.output}" }
    },
    {
      "id": "catalog",
      "type": "mediacms.register",
      "typeVersion": 1,
      "parameters": {
        "assetId": "${nodes.register.output.id}",
        "master": "${nodes.store_master.output}",
        "index": "${nodes.index.output}"
      }
    }
  ],
  "connections": {
    "register": ["backup", "compress", "transcribe"],
    "compress": ["store_master"],
    "transcribe": ["index"],
    "backup": ["catalog"],
    "store_master": ["catalog"],
    "index": ["catalog"]
  },
  "settings": { "retry": 3, "resumeOnFailure": true, "saveExecutionProgress": true }
}
```

**Atenção:** esse é um exemplo mínimo para explicar o formato; a implementação completa deve incluir proxy, resumos, geração de embeddings, validações, `ready_for_clipping`, eventos, estados e checksums. O registro no MediaCMS pode requerer integração específica à versão instalada.

**(v1.1, R26) Diferenças do exemplo em relação a §3.1, que a implementação deve resolver:**

- falta o nó `audio.extract` (etapa 8) entre a fonte e `ai.transcribe`;
- a chave `originais/<id>/original.mp4` fixa a extensão `.mp4`, mas o bruto pode ser MOV/MKV/MXF etc. A chave deve preservar a extensão original (`original.<ext>`) e a versão (§14.1);
- o nó `catalog` (MediaCMS) não deve ser pré-condição de `ready_for_clipping` (R11); falta o nó que muda o estado para `ready_for_clipping` dependendo de `backup`, `store_master`, proxy e `index`.

### 6.4 Exemplo ilustrativo: corte

```json
{
  "id": "create-clip",
  "name": "Create Semantic Clip",
  "version": "1.0.0",
  "inputs": {
    "source_id": { "type": "string", "required": true },
    "prompt": { "type": "string", "required": true }
  },
  "nodes": [
    {
      "id": "source",
      "type": "catalog.get",
      "typeVersion": 1,
      "parameters": {
        "id": "${inputs.source_id}",
        "requiredStatus": "ready_for_clipping"
      }
    },
    {
      "id": "find_story",
      "type": "ai.find_segment",
      "typeVersion": 1,
      "parameters": {
        "asset": "${nodes.source.output}",
        "query": "${inputs.prompt}",
        "mode": "complete_story"
      }
    },
    {
      "id": "preview",
      "type": "video.preview",
      "typeVersion": 1,
      "parameters": {
        "source": "${nodes.source.output.proxy_uri}",
        "segments": "${nodes.find_story.output.segments}"
      }
    },
    {
      "id": "approve_selection",
      "type": "approval.request",
      "typeVersion": 1,
      "parameters": {
        "kind": "clip_selection",
        "assetId": "${inputs.source_id}",
        "evidence": "${nodes.find_story.output}",
        "preview": "${nodes.preview.output.file}",
        "allowAdjust": true
      }
    },
    {
      "id": "render",
      "type": "video.cut",
      "typeVersion": 1,
      "parameters": {
        "source": "${nodes.source.output.working_master_uri}",
        "segments": "${nodes.approve_selection.output.segments}"
      }
    },
    {
      "id": "ingest_derived",
      "type": "workflow.execute",
      "typeVersion": 1,
      "parameters": {
        "workflow": "ingest-derived",
        "file": "${nodes.render.output.file}",
        "kind": "clip",
        "lineage": {
          "method": "computed",
          "source_asset_id": "${inputs.source_id}",
          "segments": "${nodes.approve_selection.output.segments}"
        }
      }
    },
    {
      "id": "publish",
      "type": "workflow.execute",
      "typeVersion": 1,
      "parameters": {
        "workflow": "publish-video",
        "assetId": "${nodes.ingest_derived.output.asset_id}",
        "destinations": ["youtube-main"],
        "requireApproval": true
      }
    }
  ],
  "connections": {
    "source": ["find_story"],
    "find_story": ["preview"],
    "preview": ["approve_selection"],
    "approve_selection": ["render"],
    "render": ["ingest_derived"],
    "ingest_derived": ["publish"]
  }
}
```

**(v1.1, R27)** Em relação à v1.0, o exemplo passou a incluir a prévia a partir do proxy (§3.2, etapa 6), o portão de seleção com ajuste de início/fim (etapa 7), os segmentos **aprovados**, e não os sugeridos, como entrada da renderização, e a linhagem completa passada a `ingest-derived` (antes só `parentId` era passado, e os intervalos se perdiam). A produção editorial (etapa 12) e o portão de publicação ficam dentro de `publish-video`.

**Invariante do nó `video.cut`:** a URL recebida deve ser validada como pertencente ao papel lógico `master` (ou cache local proveniente dele); é proibido fallback silencioso para `archive`. **(v1.1, R28)** Para que essa validação seja possível, os papéis precisam ocupar **namespaces disjuntos**: dois papéis não podem apontar para o mesmo bucket com prefixos sobrepostos (na v1.0, `master` usava `videos-standard` sem prefixo e `proxy` usava `videos-standard/proxies/`, ou seja, todo proxy também estava “dentro” do master). O validador de configuração rejeita sobreposição (§7.2). O cache local do worker guarda o papel e o checksum de origem, e `video.cut` confere os dois.

### 6.5 Versionamento e execução segura

- Salvar **snapshot imutável** da versão do JSON junto a cada job.
- Novas versões do fluxo não alteram jobs em andamento.
- Validar nomes de nós, dependências, ciclos, schemas, permissões e capacidades dos conectores.
- Sem strings que virem comandos de shell por concatenação; não expor avaliação de código arbitrário por padrão.
- Definir forma explícita para espera de aprovação e timeout.
- Exportar/importar workflows e schemas sem incluir credenciais.
- **(v1.1, R21)** O snapshot do workflow também entra, por hash, no resumo de proveniência permanente do ativo gerado.

---

## 7. Arquitetura de conectores e plugins

### 7.1 Regra geral [DECIDIDO]

**O núcleo define contratos; conectores integram serviços externos; nós executam operações; workflows organizam nós; plugins adicionam nós, conectores, hooks e assinantes de eventos.**

- O projeto não pode acoplar o core a GCS, YouTube, Loki, MediaCMS ou um provedor de IA específico.
- Configurações de conectores fora dos workflows: nomes lógicos, parâmetros, capacidades, `credentialsRef`.
- Conectores devem declarar versão, schema, capacidades, limites e eventual custo/semântica relevante.
- A alteração de um provedor, quando equivalente, não deverá exigir editar cada workflow JSON.
- **A migração dos arquivos existentes continua sendo uma operação separada**; alterar a configuração não migra os dados automaticamente.

### 7.2 Storage connectors [DECIDIDO]

Primeiro conector: **Google Cloud Storage**. Futuras extensões: **AWS S3, Dropbox, SFTP, Azure Blob, local/NAS** e outros.

Papéis lógicos:

- `archive`: preservação/backup, não usado para produzir cortes.
- `master`: master operacional, acesso direto, permanente durante o ciclo de criação de cortes.
- `proxy`: reprodução, preview e navegação.

**Mapeamento inicial:**

| Papel | Conector inicial | Classe | Conteúdo |
|---|---|---|---|
| `archive` | GCS | ARCHIVE | Originais brutos e masters finais dos cortes |
| `master` | GCS | STANDARD | Masters operacionais de originais e derivados e arquivos publicados (v1.1, R4) |
| `proxy` | GCS | STANDARD | Proxies, thumbnails e prévias |

**(v1.1, R4)** Na v1.0, a tabela colocava “mídias publicadas” em `proxy`, enquanto §3.4 e §7.3 exigiam guardar o arquivo publicado no storage operacional permanente e §14.1 o colocava junto ao master. Ficou definido que o arquivo publicado fica em `master`, que tem a garantia de permanência; o `proxy` pode ter uma política de retenção mais curta.

**GCS Standard não é acesso sem custos:** podem existir custos de armazenamento, requests e tráfego de saída. Evitar egress desnecessário; quando possível, executar worker próximo à região do bucket. Não usar Archive em leitura operacional.

**(v1.1, R29) [VALIDAR] Custos do Archive:** a classe ARCHIVE do GCS tem **duração mínima de armazenamento** (365 dias, segundo a documentação pública atual) com cobrança proporcional em caso de exclusão antecipada, além de custo de recuperação. Consequências para o desenho:

- as cópias de backup de cortes só são criadas **depois** do portão de seleção (o fluxo de §3.2 já garante isso: a renderização vem após a etapa 7); prévias e renderizações descartadas nunca vão para o `archive`;
- cada nova versão de mídia (R6) cria um novo objeto no Archive; recomenda-se fazer backup apenas das versões **aprovadas para publicação** ou da última versão, conforme a política `archivePolicy` (`all_versions` | `approved_versions` | `latest`, padrão **[PROPOSTO]** `approved_versions` para derivados e `all_versions` para originais);
- `delete-asset.json` (R16) informa o custo de exclusão antecipada antes de executar.

**Operações padrão:** `put`, `get`, `stat`, `list`, `exists`, `delete`, `copy`, `move`, `checksum/verify`, `downloadRange`, `capabilities`. Operações não disponíveis devem ser declaradas como tal ou implementadas por fallback explícito sujeito a custo/permissão.

**Capacidades opcionais:** versionamento, leitura por range, cópia server-side, upload resumable/multipart, verificação de checksum, classes de storage e semântica de restauração. A política de acesso direto deve ser declarada na instância de storage, não inferida apenas do nome do driver.

**Exemplo de configuração:**

```json
{
  "storages": {
    "archive": {
      "driver": "gcs",
      "bucket": "videos-archive",
      "storageClass": "ARCHIVE",
      "credentialsRef": "gcs-main",
      "usage": "backup_only"
    },
    "master": {
      "driver": "gcs",
      "bucket": "videos-standard",
      "prefix": "masters/",
      "storageClass": "STANDARD",
      "credentialsRef": "gcs-main",
      "usage": "operational"
    },
    "proxy": {
      "driver": "gcs",
      "bucket": "videos-standard",
      "prefix": "proxies/",
      "storageClass": "STANDARD",
      "credentialsRef": "gcs-main",
      "usage": "operational"
    }
  }
}
```

**(v1.1, R28)** O `master` passou a ter o prefixo `masters/`, para que `master` e `proxy` não se sobreponham no mesmo bucket. Usar buckets separados também é válido.

**(v1.1, R30) Papel × instância nos registros:** o ativo referencia o **papel** (`storage: "master"`) e a chave, como na v1.0, e acrescenta um campo `location` imutável, gravado no momento da escrita (`driver`, `bucket`, `prefix`, `generation`/versão do objeto quando houver). Assim, depois de uma migração (§15.4), a auditoria ainda sabe onde cada arquivo estava, e a ferramenta de migração consegue verificar o que já foi copiado.

**Exemplo de nó neutro:**

```json
{
  "id": "backup",
  "type": "storage.put",
  "typeVersion": 1,
  "parameters": {
    "storage": "archive",
    "source": "${inputs.file}",
    "key": "originais/${nodes.register.output.id}/original.mp4",
    "verifyChecksum": true
  }
}
```

### 7.3 Publishing connectors [DECIDIDO]

**YouTube** primeiro; futuros: **Vimeo, PeerTube, Dailymotion** e outros. Um ativo pode ter **várias publicações**, sem duplicação de identidade.

Contrato de capacidades: `upload`, `get`, `update`, `delete`, `setVisibility`, `uploadThumbnail`, `uploadCaptions`, `getStatus`, `getUrl`, `capabilities`.

- Estados e suportes de metadados variam por plataforma; exigir declaração do conector.
- Uploads devem ser retomáveis quando o destino suportar.
- Publicação não será publicamente visível sem **aprovação** por padrão.
- **(v1.1, R31) [PROPOSTO] Momento da aprovação:** a configuração de exemplo usa `visibility: "private"` como padrão do destino, e §12.4 mostra publicações `public`. Para conciliar: por padrão, a aprovação de publicação acontece **antes do upload**, porque o envio já entrega o conteúdo a um terceiro. Com a opção `allowPrivateUploadBeforeApproval: true`, é permitido enviar como `private` antes e exigir a aprovação apenas para mudar a visibilidade para `unlisted`/`public`. Mudar a visibilidade para pública é sempre um efeito externo idempotente, registrado e auditado.
- Registrar `destination`, `remote_id`, `url`, versão, estado, timestamps, título/descrição e visibilidade.
- **Reutilizar master/versão enviada** do storage `master` ou armazenamento operacional para republicar em outro destino sem recuperar backup.
- Se o destino for YouTube, guardar o arquivo utilizado no upload em storage operacional, não confiar em re-download posterior.

```json
{
  "publishing": {
    "destinations": {
      "youtube-main": {
        "driver": "youtube",
        "credentialsRef": "youtube-oauth",
        "visibility": "private"
      },
      "vimeo-main": {
        "driver": "vimeo",
        "credentialsRef": "vimeo-oauth",
        "visibility": "private"
      }
    },
    "defaults": {
      "requireApproval": true,
      "allowPrivateUploadBeforeApproval": false,
      "uploadResumable": true,
      "savePublicationRecord": true
    }
  }
}
```

### 7.4 Observability connectors [DECIDIDO]

Observabilidade é plugável, com possibilidade de **múltiplos destinos ativos simultaneamente**.

Interfaces distintas:

- `LogSink`: JSONL local, **Loki**, **Datadog**, Syslog, Google Cloud Logging etc.
- `MetricsExporter`: Prometheus, Datadog, OTLP.
- `TraceExporter`: OTLP, Datadog APM, outros.

**OpenTelemetry** é o padrão recomendado de instrumentação; exportadores desacoplados. Falha de Loki não deve afetar Datadog nem bloquear processamento.

**[PROPOSTO]** Configuração de logs:

```json
{
  "observability": {
    "logs": {
      "level": "info",
      "sinks": [
        {
          "id": "local",
          "driver": "file",
          "enabled": true,
          "parameters": {
            "path": "/var/log/vidyomd/vidyomd.jsonl",
            "rotation": "daily",
            "retentionDays": 30
          }
        },
        {
          "id": "loki",
          "driver": "loki",
          "enabled": true,
          "credentialsRef": "loki-auth",
          "parameters": { "url": "https://loki.example.com" }
        },
        {
          "id": "datadog",
          "driver": "datadog",
          "enabled": false,
          "credentialsRef": "datadog-api-key",
          "parameters": { "site": "datadoghq.com" }
        }
      ]
    }
  }
}
```

**(v1.1, R32)** O arquivo de log local passou de `events.jsonl` para `vidyomd.jsonl`, para não ser confundido com o Event Bus (§8.2), que é persistido no banco, e não em arquivo de log.

### 7.5 Outros conectores [DECIDIDO como arquitetura; implementação gradual]

- `CatalogConnector`: inicialmente **MediaCMS**.
- `TranscriptionConnector`: inicialmente **faster-whisper** local; APIs e outros motores no futuro.
- `AIProvider`: inferência local ou API remota; providers configuráveis e intercambiáveis onde suas capacidades equivalerem.
- Integração **VideoVortex** para CLI/indexação de transcrições, busca e operações de corte; confirmar compatibilidade e funcionalidades exatas da edição instalada.
- **Rewind** é **opcional** para exploração/edição visual, não dependência do MVP.
- **Arcadeia** foi considerado e **não escolhido** como componente da arquitetura prioritária.
- Next.js é a tecnologia proposta para a **biblioteca pública**, consumindo APIs/dados publicados pelo Vidyom (ver §13.2 sobre como isso funciona com um daemon em rede privada).
- **(v1.1, R33)** Status corrigido: catálogo MediaCMS = **DECIDIDO** (decisão 17). VideoVortex = **PROPOSTO E PREFERIDO** (decisão 18; em §13.1 aparecia sob um título [DECIDIDO]). Rewind e Arcadeia = **PROPOSTO** (decisão 19). **[VALIDAR]** Licenças de MediaCMS e VideoVortex e seu efeito sobre a licença do Vidyom: integrar por API/CLI como serviço separado tende a não contaminar o código do Vidyom, mas isso precisa ser confirmado juntamente com o item 1 de §17.

### 7.6 Manifesto de plugins [PROPOSTO]

```json
{
  "id": "vidyom.connector.storage.gcs",
  "version": "1.0.0",
  "runtime": "process",
  "provides": {
    "storageDrivers": ["gcs"]
  },
  "permissions": {
    "network": ["storage.googleapis.com"],
    "secrets": ["gcs-main"],
    "filesystem": ["job-temp"]
  },
  "contracts": {
    "storage": "1.x"
  }
}
```

Plugins podem rodar em **processos isolados** comunicando-se por protocolo versionado (gRPC/JSON-RPC/HTTP), com WASM para alguns casos. Não conceder automaticamente filesystem, rede, keyring, credenciais ou execução arbitrária de comandos.

---

## 8. Hooks de ciclo de vida e Event Bus

### 8.1 Hooks [DECIDIDO]

Hooks de workflow e de nó, configuráveis por JSON, sem necessidade de modificar o engine:

| Hook/evento | Semântica proposta |
|---|---|
| `workflow.before` | Antes de começar: pode validar/bloquear |
| `workflow.start` | Início efetivo, notificações |
| `workflow.after` | Sucesso do workflow, antes de `finish` |
| `workflow.finish` | Finalização terminal, sucesso/erro/cancelamento |
| `workflow.error` | Erro, tratamento controlado |
| `workflow.cancel` | Pedido/execução de cancelamento |
| `node.before` | Antes do nó, permite validação/modificação autorizada |
| `node.after` | Nó concluiu com sucesso |
| `node.error` | Falha do nó |
| `node.finish` | Fim da tentativa do nó, inclusive erro |

**Níveis:** global (`config/hooks.json`), workflow (`hooks` no JSON) e nó (`hooks` no nó). Ordem proposta: global → workflow → nó em `before`, ordem reversa em `after`/`finish`.

**(v1.1, R34) Nomes e diferença entre hook e evento:**

- Dentro do objeto `hooks`, as chaves usam o nome curto (`before`, `start`, `after`, `finish`, `error`, `cancel`); o escopo vem da posição: `hooks` no topo do workflow = `workflow.*`; `hooks` dentro de um nó = `node.*`. Para nós, `start` e `cancel` não se aplicam.
- **Hook** é síncrono e faz parte da execução: pode bloquear (`before`) e tem timeout. **Evento** (§8.2) é assíncrono, vai para o outbox e nunca bloqueia o workflow. `workflow.after` (hook) e `workflow.completed` (evento) são coisas diferentes: o evento é emitido depois que o estado terminal é gravado.
- **Timeout padrão [PROPOSTO]:** 30 s quando `timeoutSeconds` for omitido (a v1.0 exigia timeout, mas os exemplos `after`/`finish` não o declaravam). Hooks de notificação não podem passar de um limite global configurável.

**Regras:**

- Hooks globais de segurança/auditoria não podem ser removidos por workflows.
- `finish` executa para terminação válida mesmo após retomada do daemon, dentro das garantias de persistência, não prometendo execução em caso de perda irreversível do sistema.
- Política de falha por hook: `abort`, `retry`, `continue`, quando aplicável.
- Timeout obrigatório, idempotência, permissões explícitas, logs e traces.
- Hook que modifica parâmetros deve passar novamente por validação e autorização.
- Hooks de notificação não podem bloquear indefinidamente o workflow.

**Exemplo:**

```json
{
  "hooks": {
    "before": [
      {
        "id": "validate-source",
        "plugin": "asset-validator",
        "action": "check-ready-for-clipping",
        "timeoutSeconds": 15,
        "onFailure": "abort"
      }
    ],
    "after": [
      {
        "id": "notify-review",
        "plugin": "notifications",
        "action": "send-review-notification",
        "timeoutSeconds": 10,
        "onFailure": "continue"
      }
    ],
    "finish": [
      {
        "id": "record-metrics",
        "plugin": "analytics",
        "action": "record-execution",
        "timeoutSeconds": 10,
        "onFailure": "continue"
      }
    ]
  }
}
```

### 8.2 Eventos [DECIDIDO]

Event Bus **assíncrono, persistente**, com subscriptions de plugins ou workflows:

`original.indexed`, `asset.created`, `asset.indexed`, `clip.created`, `storage.backup.verified`, `publication.completed`, `publication.created`, `workflow.completed`, `workflow.failed`, entre outros.

**(v1.1, R35) Semântica dos eventos citados [PROPOSTO]:**

| Evento | Quando é emitido |
|---|---|
| `asset.created` | ID alocado (`registered`), para qualquer tipo |
| `clip.created` | especialização de `asset.created` para `kind = clip`; ambos são emitidos |
| `asset.indexed` | índice canônico válido, para qualquer tipo |
| `original.indexed` | especialização de `asset.indexed` para originais; ambos são emitidos |
| `asset.ready_for_clipping` | condição de prontidão satisfeita (R11), que é diferente de “indexado” |
| `storage.backup.verified` | checksum do backup conferido |
| `approval.requested` / `approval.decided` | portões humanos (R25) |
| `publication.created` | registro de publicação criado, antes ou durante o upload |
| `publication.completed` | destino confirmou o processamento; o payload traz a `visibility` atual |
| `publication.updated` | visibilidade ou metadados alterados no destino |
| `publication.withdrawn` | publicação removida ou tornada privada (R15) |
| `workflow.completed` / `workflow.failed` / `workflow.cancelled` | estado terminal do job gravado |

**Envelope mínimo de evento:** `event_id` (ULID, também usado como chave de idempotência), `type`, `schema_version`, `occurred_at` (UTC, RFC 3339), `job_id`, `workflow_id`, `asset_id`, `trace_id`, `causation_id` (evento ou job que o originou) e `payload`. O `causation_id` permite medir a profundidade da cadeia e **prevenir loops** (limite configurável de profundidade, ex.: 5).

- Eventos podem disparar outros workflows (sem alterar o workflow original).
- Persistir via **transactional outbox** [PROPOSTO], com retries e event delivery tracking.
- Entrega **at-least-once**, com idempotency keys; não prometer exactly-once em destinos externos.
- Prevenir recursão infinita, loops de eventos, fan-out indiscriminado e ausência de limites.
- Controle de acesso: plugins e assinaturas só podem consumir eventos permitidos e dados devidamente filtrados.

**Exemplo:**

```json
{
  "id": "update-public-library",
  "trigger": {
    "type": "event",
    "event": "publication.completed",
    "filter": { "visibility": "public" }
  },
  "nodes": [
    {
      "id": "sync",
      "type": "catalog.public.sync",
      "typeVersion": 1,
      "parameters": { "assetId": "${event.asset_id}" }
    }
  ]
}
```

**(v1.1, R15)** Um workflow complementar assinado em `publication.withdrawn` e `publication.updated` (quando a visibilidade deixa de ser pública) remove o ativo da biblioteca pública. Sem ele, um vídeo retirado do YouTube continuaria listado.

---

## 9. Segurança e gestão de credenciais

### 9.1 Segredos e keyring [DECIDIDO]

- Credenciais **obrigatoriamente no keyring do sistema operacional**, podendo residir no **cliente ou servidor**.
- macOS: Keychain; Windows: Credential Manager; Linux desktop: Secret Service/KWallet/GNOME Keyring; Linux headless: Secret Service seguro explicitamente provisionado.
- **Não** fazer fallback silencioso para arquivo em plaintext no servidor sem keyring; se keyring não estiver configurado, falhar com erro claro ou exigir provisão segura.
- JSONs de workflows e configurações contêm somente referências `credentialsRef`, nunca tokens, secrets ou senhas.
- `client`: segredo guardado no keyring do usuário; pode não estar disponível para daemon remoto desconectado.
- `server`: segredo guardado no keyring do daemon, preferido para jobs autônomos longos.
- `delegated`: credencial temporária/escopo mínimo, enviada ao servidor por canal autenticado e criptografado, mediante consentimento/autorização; duração e armazenamento limitados.
- Segredos não aparecem em argumentos CLI, logs, traces, eventos, snapshots, manifests, arquivos exportados ou mensagens de erro. **(v1.1, R36)** Por isso `vidyom secret add` lê o valor por prompt sem eco ou por stdin, nunca por argumento ou variável de ambiente exibida no histórico. Fluxos OAuth (YouTube) são feitos pelo navegador, e o token resultante vai direto para o keyring do destino escolhido.
- **(v1.1, R37)** Jobs que rodam sem cliente conectado (publicação após aprovação, eventos, agendamentos) **só podem usar credenciais `server` ou `delegated` válidas**. O validador (`--dry-run`) avisa quando um workflow com trigger por evento ou agendamento referencia uma credencial `client`. O exemplo de §11.5 foi ajustado: `youtube-oauth` passou a `--location server`, porque a publicação acontece depois da aprovação, possivelmente sem o CLI conectado.
- Rotação, revogação, testes de acesso e auditoria de uso são requisitos.

### 9.2 Proteção da comunicação [DECIDIDO]

- Comunicação cliente-servidor **criptografada e autenticada**.
- Conexão remota: SSH ou TLS 1.3/HTTPS com validação de certificados; mTLS quando adequado.
- Unix Socket local protegido por permissões de filesystem e identidade do processo (não criptografa o tráfego por si só).
- Autenticação e **autorização por operação/ativo**, independentemente de VPN.
- **[PROPOSTO]** Papéis: `viewer`, `editor`, `publisher`, `admin`, além de permissões granulares (`storage.archive.restore`, `asset.delete`, `plugin.install`, `secret.write`).

### 9.3 Redes privadas [DECIDIDO]

- Possibilidade de rodar **exclusivamente em interface de Tailscale, NetBird ou equivalente**, sem exposição na internet pública.
- O termo “tailwind” mencionado durante a discussão foi interpretado como **Tailscale** no contexto de overlay networking.
- Modos: bind explícito a IP privado de VPN; ou `127.0.0.1` com Tailscale Serve/túnel/proxy autenticado.
- **Não** fazer fallback para `0.0.0.0` quando a interface privada não existir; falhar fechado.
- **(v1.1, R38)** A decisão 29 da §18 dizia “operação restrita à interface Tailscale/NetBird”, mas esta seção (e a própria conversa) trata isso como uma **possibilidade**. Texto da decisão corrigido para “**possibilidade** de operação restrita…”. Os modos suportados são: socket local apenas; rede privada (overlay); TLS público com autenticação. Em todos, a autenticação e o RBAC continuam obrigatórios.
- Firewall e políticas de acesso da rede privada complementam TLS, autenticação e RBAC; não os substituem.

### 9.4 Execução de plugins e workflows [DECIDIDO]

- Plugins com permissões declaradas para secrets, redes e arquivos.
- Verificar origem, hashes, assinaturas quando possível e compatibilidade de versões.
- Workflows JSON importados não herdam automaticamente permissões de todos os conectores.
- Sem shell arbitrário por padrão; passar argumentos do FFmpeg de maneira segura/validada, não concatenar comando.
- Aprovação humana antes de publicar ou deletar, por padrão para publicação; ações destrutivas com permissão diferenciada.
- Minimizar exposição de transcrições privadas e conteúdo sensível nos logs de IA e indexação.
- **(v1.1, R39)** Transcrições e áudio de pessoas identificáveis são **dados pessoais** sob a LGPD (o MapaDeva opera no Brasil) e normas equivalentes em outros países. Enviar transcrição ou áudio a um provedor de IA externo é tratamento por terceiro e precisa ser configurável **por ativo ou por coleção** (`aiProcessing: local_only | external_allowed`). O padrão **[PROPOSTO]** é `local_only` sempre que houver motor local disponível. O tema faz parte de §17, item 6.

---

## 10. Logs, métricas, traces, auditoria e alertas

### 10.1 Modelo [DECIDIDO]

- Logs estruturados JSON com `timestamp`, `level`, `service`, `event`, `job_id`, `workflow_id`, `node_id`, `asset_id`, `trace_id`, duração e status.
- Observabilidade via `Observability Manager`, com filtros, redação de segredos, buffer, batching, fan-out e exportadores plugáveis.
- Tracing distribuído entre CLI, API, daemon, workflow, worker e conector.
- Auditoria separada dos logs operacionais, durável e resistente a alterações não autorizadas.
- Logs locais acessíveis pelo CLI **mesmo sem Loki/Datadog/Grafana**.
- Destinos não disponíveis não derrubam FFmpeg ou ASR; implementar filas isoladas e retries. Eventos de auditoria críticos não podem ser descartados silenciosamente.

### 10.2 Stack inicial [PROPOSTO]

- Instrumentação: **OpenTelemetry**.
- Logs: **JSONL + Loki** inicialmente; conector Datadog planejado.
- Métricas: **Prometheus** inicialmente.
- Traces: **OTLP / Tempo** inicialmente.
- Dashboard e alertas: **Grafana** opcional, separado do daemon.
- Futuros conectores: Google Cloud Logging, Syslog e outros.

### 10.3 Métricas mínimas

- Jobs `queued`, `running`, `waiting_approval`, `failed` e `completed`.
- Tempo de execução por nó, workflow, plugin e conector.
- Repetições, falhas e latência de conectores.
- Transferência e armazenamento por papel lógico/provedor.
- CPU, GPU, RAM, espaço em disco e fila dos workers.
- Tempo/custo estimado de IA, quando mensurável.
- Originais/derivados sem backup verificado.
- Cortes aguardando revisão editorial.
- Falhas de autenticação/autorização e anomalias de segurança.
- Falhas, atraso, descartes e backlog dos exportadores de telemetria.

### 10.4 Retenção [PROPOSTO]

| Tipo | Valor inicial sugerido |
|---|---|
| Logs operacionais | 30 dias |
| Histórico de jobs/executions | 90 dias ou política administrativa |
| Auditoria | >= 1 ano, conforme exigência aplicável |
| Métricas e traces | configurável por ambiente e custo |

---

## 11. CLI especificado (interface-alvo)

Todos os comandos abaixo são **propostos para o Vidyom a ser implementado**, não funcionalidades atuais de terceiros.

### 11.1 Daemon, servidores e conexão

```bash
vidyomd start --detach
vidyom daemon status
vidyom daemon health
vidyom server add producao --ssh usuario@servidor.exemplo.com
vidyom server use producao
vidyom server list
```

### 11.2 Originais e cortes

```bash
vidyom original import aula-yoga.mp4 \
  --type yoga_class \
  --title "Aula de Hatha Yoga" \
  --workflow ingest-original

vidyom asset upload ./aula-yoga.mp4 \
  --server producao \
  --workflow ingest-original

vidyom original list
vidyom original show ORI-001
vidyom original search "respiração consciente"

vidyom clip create \
  --source ORI-001 \
  --prompt "História da mulher de 90 anos que não tinha remorso" \
  --workflow create-clip

vidyom clip preview COR-001
vidyom clip approve COR-001
vidyom clip list --source ORI-001
vidyom asset lineage COR-001

# (v1.1, R40) aprovações genéricas, ajuste e retirada
vidyom approval list --status pending
vidyom approval show APR-001
vidyom approval approve APR-001 --start 00:10:48 --end 00:18:00
vidyom approval reject APR-001 --reason "trecho incompleto"
vidyom asset versions COR-001
vidyom asset delete COR-001          # exige permissão asset.delete
```

**(v1.1, R40)** `clip approve COR-001` não era suficiente: o portão de seleção (R13) acontece **antes** de o corte existir como ativo, então ainda não há `COR-nnn` para aprovar. Os portões passam a ser tratados por `approval …` com `APR-nnn`; `clip approve COR-nnn` continua como atalho para a aprovação de publicação pendente daquele corte.

### 11.3 Workflows, nodes, jobs

```bash
vidyom workflow list
vidyom workflow validate ./workflows/create-clip.json
vidyom workflow run ./workflows/create-clip.json \
  --input source_id=ORI-001 \
  --input prompt="Caso do amigo que não recebia abraço"
vidyom workflow run ingest-original --dry-run

vidyom node list
vidyom node describe video.cut
vidyom node run ai.transcribe --asset ORI-001

vidyom job list --status running
vidyom job status JOB-001
vidyom job attach JOB-001
vidyom job logs JOB-001 --follow
vidyom job retry JOB-001
vidyom job pause JOB-001
vidyom job resume JOB-001
vidyom job cancel JOB-001
```

**(v1.1, R22)** `workflow resume JOB-001` passou a ser `job resume JOB-001`: o que se retoma é uma execução, não a definição de um workflow. Foi acrescentado `job pause`, já que o estado `paused` existia sem um comando para chegar a ele.

### 11.4 Conectores, storage e publicações

```bash
vidyom connector list
vidyom storage list
vidyom storage test master
vidyom storage verify archive --asset ORI-001
vidyom storage copy --from master --to another-storage --asset ORI-001
vidyom storage restore --asset ORI-001 --to master   # (v1.1) admin; storage.archive.restore; mostra custo e pede confirmação

vidyom publishing list
vidyom publish COR-001 --to youtube-main
vidyom publish COR-001 --to youtube-main,vimeo-main
vidyom publish status COR-001
vidyom publish sync COR-001
vidyom unpublish COR-001 --from youtube-main          # (v1.1, R15)
```

### 11.5 Credenciais, segurança, observabilidade, plugins, hooks

```bash
vidyom secret add gcs-main --location server          # valor lido por prompt/stdin (R36)
vidyom secret add youtube-oauth --location server     # (v1.1, R37) era "client"
vidyom secret add vimeo-oauth --location delegated --ttl 2h
vidyom secret list
vidyom secret test youtube-oauth
vidyom secret rotate youtube-oauth
vidyom secret delete youtube-oauth

vidyom security status
vidyom security audit --since 24h
vidyom security sessions
vidyom security revoke CLIENT-001

vidyom logs --follow
vidyom job logs JOB-001 --follow
vidyom observability list
vidyom observability test loki
vidyom observability enable datadog
vidyom observability disable loki
vidyom observability status
vidyom doctor --security
vidyom doctor --network
vidyom doctor --storage

vidyom plugin list
vidyom plugin inspect notifications
vidyom plugin install ./notifications-plugin
vidyom hook list --workflow create-clip
vidyom hook test validate-source --asset ORI-001
vidyom event list --since 24h
vidyom event watch --type clip.created
vidyom event deliveries --status failed
```

A UX final deverá definir nomes uniformes (`connector`, `publishing`, `publish`) e formato de output suportando JSON para scripts (`--json`). Logs/anexação não devem alterar o estado do job a menos que comando explícito seja usado.

---

## 12. API e modelos canônicos de dados

### 12.1 API do daemon [PROPOSTO]

Versão inicial `/api/v1` ou gRPC versionado, abrangendo:

- `POST /workflows/{id}/runs`: executar workflow.
- `GET /jobs/{id}`: estado, progresso, resultado.
- `GET /jobs/{id}/events`: streaming SSE ou equivalente.
- `POST /jobs/{id}/cancel`, `POST /jobs/{id}/retry`.
- `POST /assets/uploads`: iniciar upload retomável.
- `GET /assets/{id}`, `GET /assets/{id}/lineage`.
- `POST /clips`: solicitar corte semântico.
- `POST /approvals/{id}/approve` / `reject`.
- `POST /publications`: solicitar publicação.
- `GET /nodes`, `GET /connectors`, `GET /plugins`.
- `GET /health`, `GET /metrics` (acesso controlado).
- **(v1.1, R41)** Faltavam endpoints para operações que o CLI já tinha: `GET /search` (busca lexical+semântica, com filtro opcional por ativo), `GET /assets` (listagem/filtros), `GET /assets/{id}/versions`, `GET /approvals` e `GET /approvals/{id}`, `POST /jobs/{id}/pause` e `POST /jobs/{id}/resume`, `DELETE /publications/{id}` (retirada), `GET /events` e `GET /events/deliveries`, `GET|POST|DELETE /secrets` (só metadados; o valor nunca é devolvido), `GET /clients` e `POST /clients/{id}/revoke`, `GET /audit`.

Autenticação, autorização, idempotency keys e audit trail são obrigatórios para mutações.

### 12.2 Exemplo de ativo original

```json
{
  "id": "ORI-001",
  "kind": "original",
  "category": "yoga_class",
  "title": "Aula de Hatha Yoga",
  "visibility": "private",
  "status": "ready_for_clipping",
  "current_version": "v1",
  "master_available": true,
  "backup_verified": true,
  "catalog_sync": "synced",
  "duration_seconds": 3600,
  "language": "pt",
  "source_original": {
    "storage": "archive",
    "key": "originais/ORI-001/v1/original.mov",
    "sha256": "<hash-real-no-runtime>",
    "verified": true
  },
  "working_master": {
    "storage": "master",
    "key": "originais/ORI-001/v1/master.mp4",
    "sha256": "<hash-real-no-runtime>",
    "profile": "master-1080p@1",
    "verified": true
  },
  "proxy": {
    "storage": "proxy",
    "key": "originais/ORI-001/v1/proxy.mp4",
    "profile": "proxy-720p@1"
  },
  "transcript": {
    "format": "json+srt",
    "indexed": true,
    "timestamp_basis": "source_original",
    "engine": "faster-whisper/<modelo>",
    "embedding_model": "<modelo>@<versão>"
  },
  "provenance": {
    "job_id": "JOB-001",
    "workflow": "ingest-original@1.0.0",
    "workflow_sha256": "<hash-real-no-runtime>"
  }
}
```

A string `<hash-real-no-runtime>` é placeholder didático, não SHA-256 válido. **(v1.1, R6, R8, R21, R30)** Em relação à v1.0, foram acrescentados versão, sinalizadores de estado, perfil, modelos e proveniência; as chaves agora incluem a versão; o bruto mantém a extensão original (`.mov` no exemplo). O campo `location` (R30) foi omitido aqui para encurtar o exemplo.

### 12.3 Exemplo de corte

```json
{
  "id": "COR-001",
  "kind": "clip",
  "title": "A mulher de 90 anos que não tinha remorso",
  "parent_asset_id": "ORI-001",
  "current_version": "v1",
  "lineage": {
    "method": "computed",
    "source_asset_id": "ORI-001",
    "root_asset_id": "ORI-001",
    "selection_approval_id": "APR-001",
    "segments": [
      {
        "source_asset_id": "ORI-001",
        "source_version": "v1",
        "start_seconds": 648.0,
        "end_seconds": 1080.0
      }
    ]
  },
  "visibility": "private",
  "status": "editorial_review",
  "master": {
    "storage": "master",
    "key": "cortes/COR-001/v1/master.mp4"
  },
  "backup": {
    "storage": "archive",
    "key": "cortes/COR-001/v1/master.mp4",
    "verified": true
  },
  "publications": []
}
```

Timestamps acima são exemplos; em execução real serão calculados pela indexação. **(v1.1, R12, R42)** Cada segmento passa a indicar sua fonte e a versão dela (preparando a montagem com várias fontes) e aceita frações de segundo, porque cortes em limite de frase precisam de mais precisão que segundos inteiros. A chave do backup do corte passou de `original.mp4` para `master.mp4`: o que vai para o Archive é o **master do corte**, e chamá-lo de “original” confundia com o bruto de um `original`.

### 12.4 Exemplo de publicações independentes

```json
{
  "asset_id": "COR-001",
  "publications": [
    {
      "id": "PUB-001",
      "destination": "youtube-main",
      "asset_version": "v1",
      "published_file_sha256": "<hash-real-no-runtime>",
      "remote_id": "youtube-id-ilustrativo",
      "url": "<url-retornada-pela-api>",
      "status": "published",
      "visibility": "public",
      "approval_id": "APR-002"
    },
    {
      "id": "PUB-002",
      "destination": "vimeo-main",
      "asset_version": "v1",
      "published_file_sha256": "<hash-real-no-runtime>",
      "remote_id": "vimeo-id-ilustrativo",
      "url": "<url-retornada-pela-api>",
      "status": "published",
      "visibility": "public",
      "approval_id": "APR-002"
    }
  ]
}
```

Preencher URLs somente com respostas de APIs confirmadas. Não inventar links de publicação. **(v1.1, R43)** Na v1.0, os exemplos tinham `status: "published"` com `url: null`, uma combinação que não deve existir: `published` só é gravado depois que o destino devolve ID e URL. Enquanto não houver URL, o estado é `processing`. Estados de publicação **[PROPOSTO]:** `pending_approval`, `uploading`, `processing`, `published`, `failed`, `withdrawn`. Cada publicação registra a versão do ativo, o hash do arquivo enviado (R4) e a aprovação que a autorizou.

### 12.5 Portabilidade do acervo [DECIDIDO]

**[PROPOSTO]** Cada ativo poderá ser exportado como pacote de metadados (sem precisar exportar os masters em toda consulta):

```text
ORI-001/
├── metadata.json
├── summary.md
├── transcript.json
├── transcript.srt
├── chapters.json
├── thumbnail.jpg
├── preview.mp4            # opcional
├── lineage.json           # (v1.1) linhagem e proveniência (R21)
├── versions.json          # (v1.1) versões e checksums (R6)
└── publications.json
```

- Exportar/importar índices e documentos de forma legível, não depender do banco proprietário de MediaCMS.
- O catálogo canônico é independente de qualquer UI externa.
- Garantir UTF-8, timestamps explícitos, hashes e metadados de origem.

---

## 13. Catálogo privado e biblioteca pública

### 13.1 Privado [DECIDIDO]

- **MediaCMS**: interface visual privada, vídeos proxies, tags/categorias/coleções, navegação e integração por API quando compatível.
- **VideoVortex (`vvx`) [PROPOSTO E PREFERIDO, ver R33]**: ferramenta de indexação, pesquisa textual e operações de trecho/corte, CLI e integração com IA, conforme capacidades validadas da versão implantada.
- **PostgreSQL + pgvector [PROPOSTO]**: fonte canônica e busca semântica (embeddings) + full-text search.
- **Rewind [PROPOSTO: opcional]**: edição/revisão visual de transcrições e cortes.
- **Arcadeia [PROPOSTO: não prioritário]**: avaliado, não componente recomendado desta arquitetura.
- **(v1.1, R11)** Falhas de sincronização com MediaCMS/VideoVortex ficam em `catalog_sync`, com retry; não bloqueiam a prontidão nem a publicação.
- Verificar como MediaCMS recebe ou referencia proxy já processado; evitar duplicar transcodificação e armazenamento desnecessariamente.
- MediaCMS e VideoVortex podem manter índices próprios; o Vidyom deve sincronizar por IDs estáveis e evitar que um desses produtos vire a única fonte de verdade.

### 13.2 Público [DECIDIDO]

- Biblioteca pública separada (tecnologia proposta: **Next.js**).
- Reúne apenas ativos **publicados e aprovados**, principalmente vídeos editados e cortes.
- Player embutido ou link para YouTube, Vimeo ou outros destinos.
- Pesquisa por tema, instrutor, capítulo, história e conteúdo falado.
- Timestamps e links de posição no vídeo quando suportado pela plataforma.
- Pode exibir relações entre cortes e a gravação de origem sem expor o original privado.
- Transcrições completas ou trechos somente se revisados e explicitamente autorizados para publicação.
- Nenhuma sincronização pública automática para ativos sem estado aprovado/publicado.

**(v1.1, R44) [PROPOSTO] Como a biblioteca pública obtém os dados.** A v1.0 dizia que a biblioteca Next.js “consome APIs do Vidyom” e, ao mesmo tempo, que o daemon pode ficar restrito a uma rede privada (§9.3), sem acesso pela internet. As duas coisas são incompatíveis se a biblioteca chamar o daemon diretamente. Resolução:

- A biblioteca pública **nunca** acessa o `vidyomd`. O nó `catalog.public.sync` **envia** (push) um conjunto de dados público já filtrado para um destino próprio: um bucket com JSON/estáticos ou um banco de leitura separado, configurado como conector `PublicCatalogConnector`.
- Esse conjunto contém apenas campos liberados (título, descrição, capítulos, tags, links/embeds, trechos de transcrição autorizados) e **nenhum** caminho de storage, ID técnico interno, chave de bucket ou dado do original privado.
- A busca semântica pública usa embeddings exportados nesse mesmo conjunto (somente do conteúdo publicado) ou um índice próprio da biblioteca. Ela nunca consulta o índice privado.
- Retiradas (R15) removem o item desse conjunto de forma idempotente.

---

## 14. Organização de arquivos e repositório

### 14.1 Layout lógico de storage

```text
gs://videos-archive/                       # papel archive
  originais/
    ORI-001/v1/original.<ext>              # bruto, extensão preservada
  cortes/
    COR-001/v1/master.mp4                  # master do corte
  editados/
    EDT-001/v1/master.<ext>

gs://videos-standard/masters/              # papel master
  originais/
    ORI-001/v1/master.mp4
  cortes/
    COR-001/v1/master.mp4
    COR-001/v1/published-youtube-main.mp4  # só se diferente do master (R4)

gs://videos-standard/proxies/              # papel proxy
  originais/
    ORI-001/v1/proxy.mp4
    ORI-001/v1/thumbnail.jpg
  cortes/
    COR-001/v1/proxy.mp4
```

**Esses caminhos são convenções iniciais do conector GCS**. Workflows devem referenciar papéis lógicos `archive`, `master`, `proxy`, não buckets diretamente.

**(v1.1, R4, R6, R28, R42) Correções em relação à v1.0:** o layout da v1.0 colocava o proxy em `videos-standard/originais/…`, sem o prefixo `proxies/` que a própria configuração de §7.2 definia; as chaves não tinham versão (uma nova renderização sobrescreveria o arquivo anterior); o backup do corte se chamava `original.mp4`; não havia lugar para os ativos `edited`; e o arquivo publicado ficava duplicado mesmo quando era idêntico ao master. As chaves internas ao papel (`originais/…`) são relativas ao prefixo do papel.

### 14.2 Estrutura de código [PROPOSTO]

```text
vidyom/
├── cmd/
│   ├── vidyom/                  # CLI
│   ├── vidyomd/                 # Daemon
│   └── vidyom-worker/          # Worker
├── internal/
│   ├── api/
│   ├── auth/
│   ├── daemon/
│   ├── workflow/
│   │   ├── schema/
│   │   ├── validation/
│   │   └── executor/
│   ├── jobs/
│   ├── scheduler/
│   ├── workers/
│   ├── persistence/
│   ├── catalog/
│   ├── hooks/
│   ├── events/
│   │   ├── outbox/
│   │   └── subscriptions/
│   ├── plugins/
│   ├── security/
│   │   ├── keyring/
│   │   ├── authentication/
│   │   ├── authorization/
│   │   ├── transport/
│   │   └── audit/
│   └── observability/
│       ├── logging/
│       ├── metrics/
│       ├── tracing/
│       └── health/
├── nodes/
│   ├── storage/
│   ├── video/
│   ├── audio/
│   ├── ai/
│   ├── catalog/
│   ├── control/
│   ├── approval/                # (v1.1, R25)
│   └── integrations/
├── connectors/
│   ├── storage/
│   │   ├── gcs/
│   │   ├── s3/                  # futuro
│   │   ├── dropbox/             # futuro
│   │   ├── sftp/                # futuro
│   │   └── local/               # futuro
│   ├── publishing/
│   │   ├── youtube/
│   │   ├── vimeo/               # futuro
│   │   ├── peertube/            # futuro
│   │   └── dailymotion/         # futuro
│   ├── observability/
│   │   ├── file/
│   │   ├── loki/
│   │   ├── datadog/             # futuro
│   │   ├── prometheus/
│   │   └── otlp/
│   ├── catalog/mediacms/
│   ├── catalog/public/          # (v1.1, R44) destino do conjunto de dados público
│   ├── transcription/faster-whisper/
│   └── ai/
├── plugins/
├── workflows/
│   ├── ingest-original.json
│   ├── ingest-derived.json
│   ├── create-clip.json
│   ├── publish-video.json
│   └── unpublish-video.json      # (v1.1, R15)
├── profiles/
│   ├── master-1080p.json
│   └── proxy-720p.json
├── schemas/
├── config/
│   ├── storages.json
│   ├── publishing.json
│   ├── observability.json
│   ├── hooks.json
│   └── servers.json
├── docs/
└── tests/
```

---

## 15. Testes e critérios de aceitação

### 15.1 Cenário E2E — original

**Dado** um original de 10 GB e título/categoria, **quando** enviado pelo CLI remoto ao daemon, **então**:

1. upload é retomável e verificado por hash;
2. backup do bruto no `archive` é validado;
3. master de trabalho e proxy são gerados e armazenados no `master`/`proxy`;
4. transcrição e capítulos são pesquisáveis com timestamps;
5. mídia e metadados aparecem no MediaCMS ou interface compatível;
6. o job é observável pelo CLI mesmo após desconexão/reconexão;
7. o ativo só vira `ready_for_clipping` quando condições obrigatórias forem satisfeitas;
8. **(v1.1)** a duração do master, do proxy e do áudio de ASR bate com a do bruto dentro da tolerância (R10);
9. **(v1.1)** com o MediaCMS fora do ar, o ativo ainda atinge `ready_for_clipping` e fica com `catalog_sync = failed` (R11).

### 15.2 Cenário E2E — dois cortes em dias diferentes

**Dado** `ORI-001` já indexado, **quando** os prompts “História da mulher de 90 anos que não tinha remorso” e, dias depois, “Caso do amigo que não recebia abraço” forem enviados, **então**:

1. o índice de `ORI-001` localiza candidatos relevantes sem palavras exatas;
2. a IA propõe início e fim de cada narrativa, com revisão;
3. um worker baixa somente o **master operacional**, nunca o bruto do Archive;
4. dois novos ativos `COR-001` e `COR-002` são criados, com linhagem correta;
5. cada corte recebe backup, master operacional, proxy, transcrição derivada/revisada e índice;
6. cada corte pode ser publicado independentemente e aparecer na biblioteca pública após aprovação;
7. nenhuma etapa do Workflow A para `ORI-001` precisa rodar novamente;
8. **(v1.1)** os IDs são alocados sem colisão mesmo se os dois pedidos rodarem ao mesmo tempo (R5). Os números `COR-001`/`COR-002` são ilustrativos: se `COR-001` já existir (como no exemplo de §2.4), os novos cortes recebem os próximos números livres;
9. **(v1.1)** um ajuste de início/fim depois da renderização gera `v2` do corte sem sobrescrever `v1` (R6).

### 15.3 Cenário — YouTube como origem

- Vídeo próprio existente somente no YouTube é importado por meio autorizado e armazenado no operational storage.
- A transcrição e os cortes são feitos com a cópia local/operacional persistente.
- Depois da ingestão, criação de corte não consulta YouTube nem Archive para obter o vídeo.

### 15.4 Cenário — storage plugável

- Trocar a configuração lógica `master` de GCS para um conector S3 de teste **sem alterar o workflow**.
- Antes da troca em produção, migrar os dados e validar checksums/versões.
- Um driver sem leitura direta ou range declara a limitação; o workflow falha de modo explicável quando exigir capacidade incompatível.

### 15.5 Cenário — retomada e idempotência

- Matar/reiniciar o daemon durante FFmpeg/upload; job volta a estado coerente, com checkpoint e sem publicação duplicada.
- Falha do CLI não interrompe job.
- Duas chamadas equivalentes com mesma idempotency key não geram dois uploads/publicações.

### 15.6 Cenário — segurança

- Sem segredo no JSON, argumentos, logs, traces e arquivos exportados.
- Daemon configurado para Tailscale/NetBird não inicia listener público em caso de falha da interface.
- Cliente não autorizado não instala plugin, restaura Archive, deleta ativos ou publica.
- Linux headless sem keyring provisionado falha de forma explícita, sem plaintext silencioso.
- **(v1.1)** `server:` com `..` ou link simbólico apontando para fora de `ingestRoots` é rejeitado (R20).
- **(v1.1)** O conjunto de dados público não contém chaves de storage, IDs internos, transcrição não autorizada nem dados de originais privados (R44).
- **(v1.1)** `--dry-run` avisa quando um workflow disparado por evento usa uma credencial `client` (R37).

### 15.8 Cenário — retirada de publicação (v1.1, R15)

- `vidyom unpublish COR-001 --from youtube-main` deixa o vídeo privado ou o remove no destino, marca a publicação como `withdrawn`, emite `publication.withdrawn` e tira o corte da biblioteca pública.
- Repetir o comando não gera erro nem efeito duplicado.

### 15.7 Cenário — hooks e observabilidade

- Hook `before` pode rejeitar fonte inválida; erro auditado.
- Hook `finish` registra fim tanto por sucesso quanto por erro/cancelamento.
- Loki indisponível: jobs continuam, eventos entram em buffer com limites e registro de falha; Datadog pode continuar recebendo.
- Evento `publication.completed` dispara sincronização da biblioteca pública, com prevenção de loop e idempotência.

---

## 16. Roadmap sugerido de implementação

**Etapas abaixo são propostas, não prazos contratados.** O desenho deverá garantir compatibilidade futura sem exigir implementar todos os plugins no primeiro release.

### Fase 0 — Contratos e protótipos de integração

- Escolher licença open source, política de contribuição e nome de repositório.
- Definir JSON Schema de workflows, plugins, conectores e estados de jobs.
- PoC MediaCMS com proxy pré-processado e sincronização de metadados.
- PoC VideoVortex com transcrição, busca e corte, verificando capacidades/restrições de versão/licença.
- PoC FFmpeg de master operacional a partir de bruto e teste real de cortes.
- Validar integração com GCS, keyring Linux headless e modalidade privada Tailscale/NetBird.

### Fase 1 — Núcleo funcional e ingestão

- CLI e daemon locais/remotos, autenticação e jobs duráveis.
- Workflow JSON mínimo, `storage.put/get/verify`, `video.transcode`, `audio.extract`, `ai.transcribe`, `ai.index`.
- Conector GCS com `archive/master/proxy` e geração de metadados portáveis.
- Logs JSONL e API de job status/logs.
- `ingest-original.json` completo com `ready_for_clipping`.

### Fase 2 — Cortes semânticos e subworkflows

- Recuperação lexical+vetorial, identificação de narrativa completa.
- Preview, revisão, ajuste de timestamps e renderização com FFmpeg.
- `create-clip.json` e `ingest-derived.json` com lineage, backup e reindexação.
- Garantir que nenhum corte leia do Archive.
- **(v1.1)** Nó `approval.request`, comandos `approval …`, versões de ativo e validação de namespaces disjuntos (R6, R25, R28).

### Fase 3 — Publicação e catálogo público

- Conector YouTube; OAuth no keyring; upload retomável, status, thumbnails, legendas.
- Aprovação persistente; publicação idempotente; arquivos publicados no operacional.
- Biblioteca Next.js com busca e acesso apenas a ativos aprovados.
- **(v1.1)** Conjunto de dados público enviado por push (R44) e `unpublish-video.json` com remoção da biblioteca (R15).

### Fase 4 — Plugins, hooks e observabilidade completa

- Plugin runtime isolado, manifesto de permissões, registry.
- Hooks globais/workflow/nó; Event Bus persistente.
- OpenTelemetry, Loki, Prometheus, Tempo/Grafana opcional e interface de sinks.
- Conectores futuros: Vimeo, S3, Dropbox, SFTP, Datadog etc.

---

## 17. Decisões pendentes e validações obrigatórias

A conversa definiu a arquitetura e as interfaces de alto nível, mas alguns pontos **ainda não foram decididos explicitamente** e não devem ser preenchidos silenciosamente pela equipe:

1. **Licença do Vidyom:** MIT, Apache-2.0, AGPL ou outra; verificar compatibilidades de dependências, especialmente produtos copyleft integrados.
2. **Modelo de deployment:** Docker Compose/systemd inicial; Kubernetes e multiworker se necessário.
3. **Política exata do master operacional:** codec, bitrate/CRF e resolução por classe de original (1080p/4K), tamanho-alvo e retenção.
4. **Qual motor de embeddings/LLM e quais modelos de ASR** usar em cada ambiente; requisitos offline e custos.
5. **Política de aprovação:** níveis, usuário/editor, default de publicação e fluxos de revisão para vídeos já públicos.
6. **Políticas de direitos autorais e privacidade:** permissões para palestrantes e participantes; retirada de conteúdo, transcrições sensíveis e autorizações de publicação.
7. **Semântica final de retry, cancelamento e checkpoint** para cada tipo de nó, especialmente externos e não idempotentes.
8. **Protocolo definitivo de plugin:** gRPC, JSON-RPC, HTTP ou combinação.
9. **Formato final de expressões do workflow** e regras de merge e branching.
10. **Multiusuário e multi-tenant:** se necessários desde o MVP ou num estágio posterior.
11. **Segredos em Linux headless:** estratégia de desbloqueio do keyring em boot sem reduzir segurança.
12. **Política de retenção/segurança do cache local** para masters e arquivos temporários.
13. **Modelo de sincronização MediaCMS/VideoVortex**, tratamento de falhas e exportação de metadados.
14. **Limites e políticas de custos** para transferências GCS, transcrição e publicação.
15. **Compatibilidade API de publicação**: políticas, OAuth, limites, uploads e metadados exatos de cada plataforma.
16. **Disponibilidade e nomes de pacotes/identificadores `vidyom`** em registries, domínio e repositórios, além de verificação de marca.

**Itens acrescentados na v1.1** (lacunas encontradas na revisão; nenhum deve ser decidido em silêncio):

17. **Estratégia de segmentação para embeddings** (frase, janela com sobreposição, capítulo) e política de reindexação ao trocar de modelo (R18). Complementa o item 4.
18. **Política de versões no Archive** (`archivePolicy`, R29) e o que fazer com versões de cortes que nunca foram publicadas.
19. **Política de exclusão e retirada** (R15, R16): quem pode, prazos, o que acontece com backups sob retenção mínima e como atender pedidos de remoção (LGPD/direito de imagem). Complementa o item 6.
20. **Processamento por IA externa** (R39): padrão `local_only` ou `external_allowed`, por ativo ou coleção, e registro do consentimento.
21. **Fluxo de pareamento de clientes** (R19): mTLS ou token, validade, renovação e recuperação em caso de perda do dispositivo.
22. **Formato e hospedagem do conjunto de dados público** (R44): JSON estático, banco de leitura separado ou API própria da biblioteca; e como fazer busca semântica pública.
23. **Tolerância de alinhamento da linha do tempo** (R10) e como tratar fontes com timestamps quebrados ou taxa de quadros variável.
24. **Prefixos de ID em português** (`COR-`, `ORI-`) em um projeto internacional (R5): manter ou permitir configuração. Hoje ficam mantidos, por decisão de origem.

---

## 18. Registro consolidado de decisões da conversa

Esta seção é um índice de verificação para o time não perder decisões.

| # | Decisão | Status |
|---|---|---|
| 1 | Nome oficial **Vidyom** | DECIDIDO |
| 2 | Projeto inicialmente para **MapaDeva**, mas **open source** e de uso geral | DECIDIDO |
| 3 | Tipo de ativo/prefixo **`originais`**, e não `palestras`; “palestra” vira categoria `lecture` (v1.1, R1) | DECIDIDO |
| 4 | Conteúdo: aulas de ioga, cursos, encontros, palestras, entrevistas etc. | DECIDIDO |
| 5 | Preservar bruto em backup e usar master comprimido operacional | DECIDIDO |
| 6 | Backup inicial em **GCS Archive** | DECIDIDO |
| 7 | Master operacional inicial em **GCS Standard** | DECIDIDO |
| 8 | Proxy operacional de vídeo leve e áudio próprio para IA | DECIDIDO |
| 9 | Evitar Archive em todo o fluxo de corte | DECIDIDO |
| 10 | Importar/cachear previamente vídeos próprios que estejam somente no YouTube | DECIDIDO |
| 11 | Transcrição, capítulos, resumo, tags, timestamps, embeddings e busca semântica | DECIDIDO |
| 12 | Dois workflows independentes: ingestão de original e criação de corte | DECIDIDO |
| 13 | Input do corte = **ativo original previamente indexado + prompt** | DECIDIDO |
| 14 | IA identifica início/fim de histórias completas | DECIDIDO |
| 15 | Cada corte vira **novo ativo**, com master, backup, índice e linhagem | DECIDIDO |
| 16 | Produção editorial, aprovação e biblioteca pública para cortes publicados | DECIDIDO |
| 17 | Catálogo privado: **MediaCMS** como interface principal | DECIDIDO |
| 18 | **VideoVortex** como ferramenta de apoio de busca/CLI/cortes | PROPOSTO E PREFERIDO |
| 19 | **Rewind** opcional e **Arcadeia** não prioritário | PROPOSTO |
| 20 | Biblioteca pública inicialmente via **Next.js**, usando conteúdo aprovado/publicado | PROPOSTO E PREFERIDO |
| 21 | CLI com comandos por etapa | DECIDIDO |
| 22 | Workflows em JSON, inspirados no n8n, sem escrever código para compor nós existentes | DECIDIDO |
| 23 | Subworkflows configuráveis e reutilizáveis | DECIDIDO |
| 24 | CLI cliente `vidyom` + daemon persistente `vidyomd` | DECIDIDO |
| 25 | Worker separado `vidyom-worker`, inicialmente co-localizado | PROPOSTO |
| 26 | Arquitetura de sessão persistente similar ao Herdr | DECIDIDO |
| 27 | Comunicação local/remota criptografada e autenticada | DECIDIDO |
| 28 | Credenciais no keyring do SO, cliente **ou** servidor | DECIDIDO |
| 29 | **Possibilidade** de operação restrita à interface de **Tailscale/NetBird** (v1.1, R38) | DECIDIDO |
| 30 | Logs, métricas, traces, auditoria, alertas e OTel | DECIDIDO |
| 31 | Conectores plugáveis de Storage: GCS primeiro; S3/Dropbox/SFTP etc. | DECIDIDO |
| 32 | Conectores plugáveis de Observabilidade: Loki/Datadog etc. | DECIDIDO |
| 33 | Conectores plugáveis de Publicação: YouTube/Vimeo/etc. | DECIDIDO |
| 34 | Um ativo pode ser publicado em múltiplos destinos | DECIDIDO |
| 35 | Hooks `before`, `after`, `finish`, erros e hooks por nó | DECIDIDO |
| 36 | Plugins com permissões e eventos assíncronos persistentes | DECIDIDO |
| 37 | Arquitetura extensível para catálogo, ASR e AI providers | DECIDIDO |
| 38 | Portabilidade de metadados/exportação e importação em formatos abertos | DECIDIDO |
| 39 | Go no núcleo e Python/FFmpeg nos workers conforme necessário | PROPOSTO |
| 40 | PostgreSQL + pgvector como catálogo canônico, jobs e busca | PROPOSTO |
| 41 | (v1.1) Backup verificado faz parte de `ready_for_clipping`; sincronização com o MediaCMS não bloqueia (R11) | PROPOSTO (resolve inconsistência) |
| 42 | (v1.1) Arquivo publicado guardado no papel `master` (R4) | PROPOSTO (resolve inconsistência) |
| 43 | (v1.1) Workflow de publicação canônico `publish-video` com `destinations` (R14) | PROPOSTO (resolve inconsistência) |
| 44 | (v1.1) Dois portões de aprovação: seleção e publicação (R13, R25) | PROPOSTO |
| 45 | (v1.1) Versões imutáveis de ativo; chaves de storage com versão (R6) | PROPOSTO |
| 46 | (v1.1) Biblioteca pública recebe os dados por push e nunca acessa o daemon (R44) | PROPOSTO |
| 47 | (v1.1) Papéis de storage com namespaces disjuntos (R28) | PROPOSTO |
| 48 | (v1.1) Retirada de publicação (`unpublish-video`) e exclusão controlada (R15, R16) | PROPOSTO |

---

## 19. Resultado esperado

O Vidyom deverá permitir que qualquer usuário instale um daemon em um servidor próprio, configure storages e plataformas de publicação com credenciais guardadas de forma segura, controle todo o trabalho pelo CLI e defina pipelines de mídia por JSON.

Uma gravação de 10 GB poderá ser arquivada, convertida, transcrita, resumida e indexada **uma vez**. A partir dela, o usuário poderá pedir cortes por tema ou história em qualquer momento futuro. O daemon localizará o trecho, recuperará **somente o master operacional** do storage acessível, renderizará um novo vídeo, criará outro ativo completo com backup e metadados, disponibilizará para revisão e publicará nos destinos selecionados. Tudo isso com logs, auditoria, hooks, plugins e conectores independentes de fornecedores.

**Esse é o contrato de produto e arquitetura a partir do qual a implementação do Vidyom deverá ser planejada, dividida em issues e validada por testes de aceitação.**

---

## 20. Registro de revisão v1.1

Revisão de consistência e lacunas feita sobre a v1.0 (`docs/Vidyom_Especificacao_Completa.md`, preservada sem alterações). Tipos: **I** = inconsistência entre trechos do documento; **L** = lacuna (algo necessário que não estava especificado); **C** = esclarecimento de redação.

| # | Tipo | Onde | Problema na v1.0 | Correção na v1.1 |
|---|---|---|---|---|
| R1 | C | §2.1, §18 #3 | “Categoria `originais`” misturava tipo de ativo, prefixo de storage e categoria de conteúdo (§2.2) | Os três conceitos foram separados; “palestra” virou a categoria `lecture` |
| R2 | L | §2.1 | `edited` definido sem fluxo de entrada | Entra por `ingest-derived` com linhagem `declared` |
| R3 | L | §2.1 | Áudio de ASR temporário, sem regra de regeneração | Regenerado a partir do master, nunca do Archive |
| R4 | I | §2.1, §3.4, §7.2, §7.3, §14.1 | Arquivo publicado ficava ora em `proxy`, ora no “operacional”, ora junto ao master | Fica em `master`; sem duplicar quando é idêntico ao master |
| R5 | L | §2.3 | Prefixo de ID para `edited`/aprovação/publicação, formato e concorrência não definidos | Prefixos, numeração sem teto e alocação transacional |
| R6 | L | §1.3, §2.3, §14.1 | “Várias versões” sem modelo; chaves sem versão (sobrescrita) | Versões imutáveis `vN`; chaves com versão |
| R7 | L | §2.1 | Corte de corte não tratado | Permitido; renderização a partir do original raiz quando possível |
| R8 | L | §2, §5.5 | Estados de ativo usados mas não definidos | Tabela de estados e sinalizadores (§2.5) |
| R9 | L | §3.1 | Momento de apagar a cópia de entrada não definido | Só depois de archive e master verificados |
| R10 | L | §3.1, §12.2 | Nada garantia que os timestamps da transcrição valessem para master e proxy | Mesma linha do tempo, validada por `ffprobe` |
| R11 | I | §3.1, §13.1, §15.1 | Condição de prontidão omitia backup e proxy; o MediaCMS ficava implicitamente bloqueante, contradizendo “nenhum produto é fonte única” | Condição consolidada; sincronização de catálogo não bloqueia |
| R12 | I | §2.1, §3.2 | Corte de “um ou mais” ativos × `create-clip` com fonte única | MVP com fonte única; várias fontes como [FUTURO]; linhagem por segmento |
| R13 | C | §3.2 | Duas aprovações (etapas 7 e 13) sem distinção | Portões `clip_selection` e `publication`; aviso de sobreposição |
| R14 | I | §3.2, §3.3 | `publish-video` × `publish-youtube` | `publish-video` canônico com `destinations` |
| R15 | L | §3.3, §8.2, §13.2 | Nenhum fluxo de retirada; biblioteca não removia itens | `unpublish-video`, evento `publication.withdrawn`, comando `unpublish` |
| R16 | L | §3.3, §9.2 | Permissão `asset.delete` sem fluxo | `delete-asset.json` controlado |
| R17 | L | §4.1 | Sem regra contra aumento de resolução; áudio do master não especificado | Teto de resolução, áudio em alta qualidade, perfis versionados |
| R18 | L | §4.4, §17 #4 | Modelo de embeddings e segmentação não versionados | Vetores com modelo/versão; reindexação obrigatória ao trocar |
| R19 | L | §5.3, §9.2, §11.5 | Autenticação exigida sem fluxo de registro de cliente | Pareamento proposto, `SO_PEERCRED` local, `CLIENT-nnn` |
| R20 | I/L | §5.4, §11.2 | `original import <arquivo>` sem dizer se o arquivo está no cliente ou no servidor; sem limite de caminhos | URIs `server:`/`upload:`/`storage://`/`import:`; `ingestRoots` |
| R21 | I | §5.5, §6.5, §10.4 | Jobs retidos por 90 dias × linhagem permanente | Resumo de proveniência permanente no ativo |
| R22 | I | §5.5, §11.3 | Estado `paused` sem comando; `workflow resume JOB-…` agindo sobre um job | `job pause`/`job resume` |
| R23 | C | §11 | `JOB-001` sem regra de ID | Mesma regra de R5 |
| R24 | L | §6.2 | Referência a nó não ancestral e escape de `${` não definidos | Validação de ancestralidade; `$${` |
| R25 | L | §6.2, §7.3 | `requireApproval`/`waiting_approval` sem nó que os implementasse | Nó `approval.request` |
| R26 | I | §6.3 | Exemplo sem `audio.extract`, com `.mp4` fixo e MediaCMS antes da prontidão | Diferenças listadas para a implementação |
| R27 | I | §6.4 | Exemplo de corte sem prévia/aprovação; linhagem perdida (`parentId` apenas) | Exemplo reescrito |
| R28 | I | §6.4, §7.2 | `master` sem prefixo e `proxy` em `proxies/` no mesmo bucket: proxy “dentro” do master, impedindo a validação do papel | Prefixo `masters/`; namespaces disjuntos obrigatórios |
| R29 | L | §7.2 | Duração mínima e custo de recuperação da classe ARCHIVE ignorados para versões de cortes | `archivePolicy`; [VALIDAR] |
| R30 | L | §7.1, §15.4 | Registro só pelo papel perdia a localização física após uma migração | Campo `location` imutável |
| R31 | I | §7.3, §12.4 | Visibilidade padrão `private` × aprovação “antes da publicação pública” sem momento definido | Aprovação antes do upload; opção de upload privado prévio |
| R32 | C | §7.4 | Log `events.jsonl` confundível com o Event Bus | `vidyomd.jsonl` |
| R33 | I | §7.5, §13.1, §18 | VideoVortex sob título [DECIDIDO] × “PROPOSTO E PREFERIDO” na §18; licenças não avaliadas | Status alinhados; [VALIDAR] licenças |
| R34 | I/L | §8.1 | Chaves de hook curtas × nomes `workflow.*`; hook × evento; exemplos sem o timeout obrigatório | Mapeamento, distinção e timeout padrão |
| R35 | L | §8.2 | Eventos sem semântica (`original.indexed` × `asset.indexed`) e sem envelope; prevenção de loop sem mecanismo | Tabela de semântica, envelope, `causation_id` |
| R36 | L | §9.1, §11.5 | Como `secret add` recebe o valor sem expô-lo | Prompt sem eco ou stdin; OAuth pelo navegador |
| R37 | I | §9.1, §11.5 | `youtube-oauth --location client` × publicação autônoma depois da aprovação | `server`; aviso no `--dry-run` |
| R38 | I | §9.3, §18 #29 | “Possibilidade” virou “operação restrita” na tabela | Redação corrigida |
| R39 | L | §9.4, §17 #6 | LGPD e envio a IA externa não tratados | `aiProcessing` por ativo ou coleção |
| R40 | I | §11.2, §12.1 | `clip approve COR-…` antes de o corte existir; sem comandos de aprovação | `approval …`, `asset versions`, `asset delete` |
| R41 | L | §12.1 | API sem busca, aprovações, eventos, segredos, clientes, auditoria | Endpoints acrescentados |
| R42 | I | §12.3, §14.1 | Backup do corte chamado `original.mp4`; segmentos sem fonte e só em segundos inteiros | `master.mp4`; segmento com fonte/versão; frações de segundo |
| R43 | I | §12.4 | `status: published` com `url: null` | `published` só com URL; estados de publicação |
| R44 | I | §7.5, §9.3, §13.2 | Biblioteca pública “consome a API” × daemon restrito à rede privada | Push de um conjunto de dados público filtrado; a biblioteca nunca acessa o daemon |

**Seções novas:** §2.5 (estados de ativo), §15.8 (retirada de publicação), itens 17–24 de §17, decisões 41–48 de §18 e esta §20.
