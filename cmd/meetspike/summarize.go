package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/gtrindade/ultra-kiew/internal/config"
	"github.com/gtrindade/ultra-kiew/internal/googlegenai"
	"github.com/gtrindade/ultra-kiew/internal/storage"
)

// narrativeSummaryPrompt is the thing Phase 5 actually hinges on: can a raw,
// messy, bilingual transcript be reduced to "what happened in the story" while
// dropping table talk, without inventing anything that was not said.
//
// This is deliberately blunt about what to exclude, because the failure mode
// worth testing for is not "misses some lore" -- it is "includes a paragraph
// about someone's microphone not working" or "quotes a rules argument as if it
// were plot". Both would make the eventual auto-posted recap actively
// embarrassing rather than just mediocre.
const narrativeSummaryPrompt = `Você vai receber a transcrição bruta e desorganizada de uma sessão de RPG de mesa, feita por transcrição automática (tem erros de reconhecimento, trechos em inglês misturados, falas cortadas e repetidas).

Sua tarefa é escrever um resumo APENAS do que aconteceu NA HISTÓRIA (na ficção): decisões dos personagens, combates e seus resultados, NPCs encontrados, itens ou informações importantes obtidas, reviravoltas, e onde a sessão parou (gancho para a próxima).

NÃO INCLUA:
- Conversa fora do personagem (fora da ficção): problemas técnicos, microfone, câmera, brincadeiras entre os jogadores sobre a vida real, comentários sobre regras do sistema, combinar horário da próxima sessão, etc.
- Qualquer coisa que pareça ser os jogadores falando ENQUANTO jogam, mas que não é parte da história em si.
- Invenções: se não está claro o que aconteceu por causa de erro de transcrição, não invente um evento. É melhor omitir do que inventar.

Escreva em português brasileiro, em prosa corrida organizada por ordem cronológica dos acontecimentos (não em bullet points soltos), como se fosse o resumo que um mestre de RPG escreveria para lembrar o grupo "no capítulo anterior". Tamanho alvo: um ou dois parágrafos curtos, a menos que a sessão realmente tenha muito conteúdo relevante para a história.

Aqui está a transcrição:

---
%s
---`

func runSummarize(ctx context.Context, cfg *config.Config, transcriptPath, model string) {
	data, err := os.ReadFile(transcriptPath)
	if err != nil {
		fail(fmt.Errorf("could not read %s: %w", transcriptPath, err))
	}
	transcript := strings.TrimSpace(string(data))
	if transcript == "" {
		fail(fmt.Errorf("%s is empty", transcriptPath))
	}

	fmt.Printf("Loaded %s (%d bytes, ~%d lines)\n", transcriptPath, len(data), strings.Count(transcript, "\n")+1)

	storageClient := storage.NewClient()

	// GenerateText is stateless and carries no tools -- exactly what a
	// transcript, which is untrusted input, should be handed to. Nothing a
	// player said at the table should be able to make the model call a real
	// tool; the only thing this path can ever produce is a string.
	aiClient, err := googlegenai.NewClient(ctx, nil, storageClient, nil, cfg)
	if err != nil {
		fail(fmt.Errorf("failed to create genai client: %w", err))
	}

	prompt := fmt.Sprintf(narrativeSummaryPrompt, transcript)

	fmt.Printf("\nAsking %s for a narrative-only summary...\n", model)
	start := time.Now()
	summary, err := aiClient.GenerateTextWithModel(ctx, model, prompt)
	if err != nil {
		fail(fmt.Errorf("summary generation failed: %w", err))
	}

	fmt.Printf("\n=== SUMMARY from %s (generated in %s) ===\n\n%s\n\n=== END SUMMARY ===\n", model, time.Since(start).Round(time.Second), summary)
}
