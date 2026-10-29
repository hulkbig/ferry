package management

import (
	"sort"

	"ferry/internal/config"
	"ferry/internal/domain"
)

// This is evidence from one successful sync, returned alongside its revision
// and timestamps. It is not persisted account entitlement or routing state.
type codexCatalogSyncEvidence struct {
	ClientVersion  string                   `json:"client_version"`
	BaselineSource string                   `json:"baseline_source"`
	BaselineSHA256 string                   `json:"baseline_sha256"`
	Models         []codexModelSyncEvidence `json:"models"`
}

type codexModelSyncEvidence struct {
	ConnectionID              string `json:"connection_id"`
	Model                     string `json:"model"`
	Visibility                string `json:"visibility,omitempty"`
	ModelPriority             string `json:"model_priority"`
	ModelMinimalClientVersion string `json:"model_minimal_client_version,omitempty"`
	AccountPriority           string `json:"account_priority"`
	ConnectionPriority        bool   `json:"connection_priority"`
}

func priorityEvidenceStatus(priority *bool) string {
	if priority == nil {
		return "unknown"
	}
	if *priority {
		return "supported"
	}
	return "unsupported"
}

func codexSyncEvidence(file *config.File, providerID string, catalogs []providerModelCatalog) *codexCatalogSyncEvidence {
	evidence := &codexCatalogSyncEvidence{
		ClientVersion: file.EffectiveCodexClientVersion(), BaselineSource: domain.CodexFastBaselineSource,
		BaselineSHA256: domain.CodexFastBaselineSHA256, Models: []codexModelSyncEvidence{},
	}
	for _, catalog := range catalogs {
		connection := providerConnection(file, providerID, catalog.connectionID)
		if connection == nil {
			continue
		}
		// Report configured models, including new list-visible additions.
		// Unconfigured hidden models remain internal catalog metadata.
		models := map[string]codexModelSyncEvidence{}
		for _, model := range connection.Models {
			entry := catalog.entries[model.UpstreamModel]
			row, exists := models[model.UpstreamModel]
			if !exists {
				row = codexModelSyncEvidence{
					ConnectionID: connection.ID, Model: model.UpstreamModel, Visibility: entry.Visibility,
					ModelPriority: "unknown", AccountPriority: priorityEvidenceStatus(entry.Priority),
				}
				if baseline, known := domain.CodexFastBaseline(model.UpstreamModel); known {
					row.ModelPriority = priorityEvidenceStatus(&baseline.Priority)
					row.ModelMinimalClientVersion = baseline.MinimalClientVersion
				}
			}
			row.ConnectionPriority = row.ConnectionPriority ||
				(model.Capabilities.Priority && domain.HasProtocol(model.Protocols, domain.ProtocolOpenAIResponses))
			models[model.UpstreamModel] = row
		}
		for _, row := range models {
			evidence.Models = append(evidence.Models, row)
		}
	}
	sort.Slice(evidence.Models, func(i, j int) bool {
		a, b := evidence.Models[i], evidence.Models[j]
		if a.ConnectionID != b.ConnectionID {
			return a.ConnectionID < b.ConnectionID
		}
		return a.Model < b.Model
	})
	return evidence
}
