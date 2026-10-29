package management

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/oauth"
)

// Resolve account evidence while preparing the reviewable draft, never during
// apply or in the request scheduler. Name-only UI/API additions then use the
// same contract as periodic sync without waiting for its next run.
func (s *Server) enrichCodexDraftCapabilities(before, after *config.File) error {
	if s.deps.OAuth == nil {
		return nil // Pre-auth setup retains explicit/built-in declarations.
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	s.credentialMu.RLock()
	defer s.credentialMu.RUnlock()
	catalogs := map[string][]oauth.CatalogModel{}
	for index := range after.Connections {
		connection := &after.Connections[index]
		provider := findProvider(after, connection.ProviderID)
		if provider == nil || provider.Type != domain.ProviderCliproxyCodex || len(connection.Models) == 0 {
			continue
		}
		old := providerConnection(before, connection.ProviderID, connection.ID)
		if old != nil && old.Credential == connection.Credential && reflect.DeepEqual(old.Models, connection.Models) {
			continue // Unrelated edits must not depend on upstream availability.
		}
		account, ok := domain.OAuthAccountFromRef(connection.Credential.SecretRef)
		if !ok {
			continue
		}
		if _, active := s.deps.OAuth.Account(account); !active {
			continue
		}
		catalog, fetched := catalogs[account]
		if !fetched {
			var err error
			catalog, err = s.deps.OAuth.ReadModelCatalogWithVersion(ctx, account, after.EffectiveCodexClientVersion())
			if err != nil {
				return fmt.Errorf("connection %q: Codex model catalog unavailable; retry the model edit after catalog access recovers", connection.ID)
			}
			catalogs[account] = catalog
		}
		positions := map[string][]int{}
		for i, model := range connection.Models {
			positions[model.UpstreamModel] = append(positions[model.UpstreamModel], i)
		}
		for _, entry := range catalog {
			indexes := positions[entry.Slug]
			if len(indexes) == 0 {
				continue
			}
			// Resolve the native wire from account metadata rather than a
			// name-only editor's Responses default, including on round-trips.
			protocol, _ := codexSyncedModelPlan(entry.Slug, entry, true, domain.ProtocolOpenAIResponses)
			for _, i := range indexes {
				connection.Models[i].Protocols = []domain.Protocol{protocol}
				if protocol == domain.ProtocolOpenAIImages {
					connection.Models[i].Capabilities = domain.Capabilities{Images: true}
				}
			}
			applyCodexCatalogCapabilities(connection, indexes, entry.Slug, entry, map[string]bool{})
		}
	}
	return nil
}
