package gateway

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/luca/llm-protocol-gateway/internal/domain"
)

const qoderSampleListing = `{
  "app": [
    {"key":"ultimate","enable":true,"display_name":"Ultimate","max_input_tokens":180000,"is_vl":true,
     "thinking_config":{"enabled":{"efforts":{"low":{},"high":{"is_default":true},"max":{}}},"disabled":{}}},
    {"key":"fast","enable":true,"display_name":"Fast","max_input_tokens":180000},
    {"key":"auto","enable":true,"display_name":"Smart Routing"},
    {"key":"dropped","enable":false,"display_name":"Dropped"}
  ],
  "chat": [
    {"key":"ultimate","enable":true,"display_name":"Ultimate"},
    {"key":"performance","enable":true,"display_name":"Performance","max_input_tokens":180000,
     "thinking_config":{"enabled":{"efforts":{"medium":{"is_default":true}}}}},
    {"key":"efficient","enable":true,"display_name":"Efficient","max_input_tokens":180000},
    {"key":"qmodel_38max","enable":true,"display_name":"Qwen3.8-Max","max_input_tokens":180000,
     "context_config":{"200k":{"token_count":200000,"is_default":true},"400k":{"token_count":400000},"1m":{"token_count":1000000}}}
  ]
}`

func qoderParseSample(t *testing.T, raw string) []qoderModelInfo {
	t.Helper()
	var listing qoderListing
	if err := json.Unmarshal([]byte(raw), &listing); err != nil {
		t.Fatal(err)
	}
	return qoderListingModels(listing)
}

func keysOf(models []qoderModelInfo) []string {
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.Key)
	}
	return out
}

// Qoder's app scene is what the app shows and what a request names; a model only
// the chat scene still lists is kept, but inserted where it followed in the chat
// order rather than appended — so a model picked before stays served after the
// app re-shelf (plugins #97: the app listed Fast, 0.8x, where chat lists
// Performance, 1.1x). Fast, which only the app names, ends up behind the chat
// run that was inserted at its position.
func TestQoderListingModelsMergesTheTwoScenes(t *testing.T) {
	got := strings.Join(keysOf(qoderParseSample(t, qoderSampleListing)), ",")
	want := "ultimate,performance,efficient,qmodel_38max,fast"
	if got != want {
		t.Fatalf("merged order = %q, want %q", got, want)
	}
}

// "auto" and "default" route between models inside Qoder and are not one model
// a client can pick; a disabled entry is not either.
func TestQoderListingModelsDropsAggregatesAndDisabled(t *testing.T) {
	for _, model := range qoderParseSample(t, qoderSampleListing) {
		if model.Key == "auto" || model.Key == "dropped" {
			t.Fatalf("%q should not be listed", model.Key)
		}
	}
}

func TestQoderListingModelReadingThinkingAndVision(t *testing.T) {
	models := qoderParseSample(t, qoderSampleListing)
	var ultimate, performance, fast qoderModelInfo
	for _, model := range models {
		switch model.Key {
		case "ultimate":
			ultimate = model
		case "performance":
			performance = model
		case "fast":
			fast = model
		}
	}
	// Efforts come from thinking_config, ordered lowest first, with the default.
	if strings.Join(ultimate.Efforts, ",") != "low,high,max" {
		t.Fatalf("efforts = %v", ultimate.Efforts)
	}
	if ultimate.DefaultEffort != "high" {
		t.Fatalf("default effort = %q", ultimate.DefaultEffort)
	}
	// A thinking_config that carries a disabled block at all means thinking can
	// be turned off; only a model with none is forced to think.
	if !ultimate.Thinks || ultimate.AlwaysThinks {
		t.Fatalf("Ultimate should be able to think or not: %+v", ultimate)
	}
	if !performance.AlwaysThinks {
		t.Fatalf("a listing without a disabled block always thinks: %+v", performance)
	}
	if !ultimate.Images {
		t.Fatal("is_vl should mark the model as taking images")
	}
	if len(performance.Efforts) != 1 || performance.Efforts[0] != "medium" {
		t.Fatalf("performance efforts = %v", performance.Efforts)
	}
	// A model listed without a thinking_config but with is_reasoning false is
	// not a thinking model at all.
	if fast.Thinks {
		t.Fatal("Fast should not offer thinking")
	}
	// The whole upstream entry is kept, because a request carries it verbatim.
	if len(ultimate.Config) == 0 || !strings.Contains(string(ultimate.Config), "display_name") {
		t.Fatalf("model configuration not preserved: %s", ultimate.Config)
	}
}

// Qoder's Context setting (200K / 400K / 1M) lives in context_config;
// max_input_tokens is only what a request that names no window gets.
func TestQoderListingModelWindowsComeFromContextConfig(t *testing.T) {
	models := qoderParseSample(t, qoderSampleListing)
	var qwen qoderModelInfo
	for _, model := range models {
		if model.Key == "qmodel_38max" {
			qwen = model
		}
	}
	if strings.Join(intsToStrings(qwen.Windows), ",") != "200000,400000,1000000" {
		t.Fatalf("windows = %v", qwen.Windows)
	}
	if qwen.DefaultWindow != 200000 {
		t.Fatalf("default window = %d", qwen.DefaultWindow)
	}
	if qwen.largestContext() != 1000000 {
		t.Fatalf("the catalog should advertise the largest window, got %d", qwen.largestContext())
	}
}

func intsToStrings(values []int) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, strings.TrimSpace(jsonNumber(value)))
	}
	return out
}

func jsonNumber(value int) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func TestQoderCatalogModelsFromTheAccountListing(t *testing.T) {
	models := qoderCatalogModels("qoder", nil, qoderParseSample(t, qoderSampleListing))
	if len(models) != 5 {
		t.Fatalf("catalog = %d models", len(models))
	}
	if models[0].ID != "ultimate" || models[0].ProviderID != "qoder" || models[0].Protocol != domain.ProtocolOpenAIChat {
		t.Fatalf("first row wrong: %+v", models[0])
	}
	if !models[0].InMenu {
		t.Fatal("an account's routable models belong in the menu")
	}
	for _, model := range models {
		if model.ID != strings.ToLower(model.ID) {
			t.Errorf("model %q must be addressed by Qoder's own key form", model.ID)
		}
	}
	// The windowed model is listed with its largest window, not the 180K a
	// request naming none gets.
	for _, model := range models {
		if model.ID == "qmodel_38max" && model.ContextLength != 1000000 {
			t.Fatalf("context = %d, want the largest offered window", model.ContextLength)
		}
	}
}

// The row keeps Qoder's key as the id a request sends and carries the name
// Qoder shows its users, so the console can read "Qwen3.8-Max" instead of
// "qmodel_38max".
func TestQoderCatalogModelsCarryTheUpstreamNames(t *testing.T) {
	models := qoderCatalogModels("qoder", nil, qoderParseSample(t, qoderSampleListing))
	names := map[string]string{}
	for _, model := range models {
		names[model.ID] = model.Name
	}
	if names["qmodel_38max"] != "Qwen3.8-Max" {
		t.Fatalf("listing display_name not carried: %+v", names)
	}
	if names["ultimate"] != "Ultimate" {
		t.Fatalf("ultimate name = %q", names["ultimate"])
	}
	// The fallback catalog is named too: a connection that has not read its
	// listing yet should not read as a wall of codes either.
	for _, model := range qoderCatalogModels("qoder", qoderStaticModelsFor(qoderGlobalSite), nil) {
		if model.Name == "" {
			t.Fatalf("static entry %q has no name", model.ID)
		}
	}
}

// Before an account is signed in — or when its listing cannot be asked — the
// site's own catalog keeps the console usable. The two sites differ.
func TestQoderStaticCatalogPerSite(t *testing.T) {
	global := qoderCatalogModels("qoder", qoderStaticModelsFor(qoderGlobalSite), nil)
	cn := qoderCatalogModels("qoder-cn", qoderStaticModelsFor(qoderCNSite), nil)
	if len(global) < len(cn) {
		t.Fatalf("the international catalog should be the fuller one: %d vs %d", len(global), len(cn))
	}
	var cnHasLite, globalHasLite bool
	for _, model := range cn {
		if model.ID == "lite" {
			cnHasLite = true
		}
	}
	for _, model := range global {
		if model.ID == "lite" {
			globalHasLite = true
		}
	}
	if !cnHasLite || globalHasLite {
		t.Fatalf("Lite belongs to Qoder CN's CLI tiers only (cn=%v global=%v)", cnHasLite, globalHasLite)
	}
}

// A request has to name the model with the configuration the listing gave it;
// an unknown or unlisted model is not routable.
func TestQoderModelForReadsTheStoredListing(t *testing.T) {
	provider := domain.Provider{
		ID:       "qoder",
		AuthType: domain.AuthTypeQoderPAT,
		QoderPAT: &domain.QoderCredential{Models: json.RawMessage(qoderSampleListing)},
	}
	info, ok := qoderModelFor(provider, "Performance")
	if !ok {
		t.Fatal("a model the account lists must be found regardless of case")
	}
	if info.Key != "performance" || len(info.Config) == 0 {
		t.Fatalf("lookup wrong: %+v", info)
	}
	if _, ok := qoderModelFor(provider, "not-a-model"); ok {
		t.Fatal("an unlisted model must not be sent upstream")
	}
	if _, ok := qoderModelFor(domain.Provider{}, "ultimate"); ok {
		t.Fatal("an account with no listing has no model")
	}
}
