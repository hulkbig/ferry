package oauth

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

func TestModelCatalogRetainsHiddenCapabilityEvidence(t *testing.T) {
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return modelCatalogResponse(http.StatusOK, `{"models":[
			{"slug":"gpt-6-astra","visibility":" HIDE ","service_tiers":[{"id":"priority"}]},
			{"slug":"visible","visibility":"hide","service_tiers":[]},
			{"slug":"visible","visibility":"list","service_tiers":[{"id":"priority"}]},
			{"slug":"unknown-visibility","service_tiers":null}
		]}`), nil
	}))
	models, err := m.ReadModelCatalog(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gpt-6-astra", "unknown-visibility", "visible"}; !reflect.DeepEqual(catalogSlugs(models), want) {
		t.Fatalf("full catalog=%v want=%v", catalogSlugs(models), want)
	}
	if astra := models[0]; astra.Visibility != "hide" || astra.Priority == nil || !*astra.Priority {
		t.Fatalf("hidden Astra lost evidence: %+v", astra)
	}
	if models[1].Priority != nil {
		t.Fatal("null tier list must remain unknown")
	}
	discovered, err := m.DiscoverModels(context.Background(), "work")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(catalogSlugs(discovered), []string{"visible"}) ||
		discovered[0].Priority == nil || !*discovered[0].Priority {
		t.Fatalf("discovery must prefer the visible duplicate: %+v", discovered)
	}
}

func TestAllHiddenCatalogIsValidButDiscoversNoModels(t *testing.T) {
	m := testCatalogManager(t, modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return modelCatalogResponse(http.StatusOK, `{"models":[{"slug":"gpt-6-astra","visibility":"hide"}]}`), nil
	}))
	models, err := m.ReadModelCatalog(context.Background(), "work")
	if err != nil || len(models) != 1 {
		t.Fatalf("full catalog=%v error=%v", models, err)
	}
	discovered, err := m.DiscoverModels(context.Background(), "work")
	if err != nil || len(discovered) != 0 {
		t.Fatalf("empty discovery must not trigger a static fallback: models=%v error=%v", discovered, err)
	}
}
